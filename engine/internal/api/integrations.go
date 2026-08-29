package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"

	"consize/internal/store"
)

const githubIntegrationStoreKey = "integrations.github"

type githubIntegrationResponse struct {
	Config       githubIntegrationConfig `json:"config"`
	Source       string                  `json:"source"`
	TokenPresent bool                    `json:"token_present"`
}

type githubIntegrationConfig struct {
	Enabled              bool               `json:"enabled"`
	Organization         string             `json:"organization"`
	TokenEnv             string             `json:"token_env"`
	DefaultRepo          string             `json:"default_repo"`
	DefaultPath          string             `json:"default_path"`
	DefaultTerraformAddr string             `json:"default_terraform_addr"`
	Repositories         []githubRepository `json:"repositories"`
}

type githubRepository struct {
	Alias         string `json:"alias"`
	Repo          string `json:"repo"`
	DefaultBranch string `json:"default_branch"`
	RootPath      string `json:"root_path"`
}

var envNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func (s *Server) getGitHubIntegration(w http.ResponseWriter, r *http.Request) {
	cfg, source, err := s.githubIntegrationConfig(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, githubIntegrationResponse{
		Config:       cfg,
		Source:       source,
		TokenPresent: githubTokenPresent(cfg),
	})
}

func (s *Server) putGitHubIntegration(w http.ResponseWriter, r *http.Request) {
	var cfg githubIntegrationConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be a GitHub integration config"})
		return
	}
	cfg = sanitizeGitHubIntegration(cfg)
	if err := validateGitHubIntegration(cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.store.PutSetting(r.Context(), githubIntegrationStoreKey, string(raw)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, githubIntegrationResponse{
		Config:       cfg,
		Source:       "store",
		TokenPresent: githubTokenPresent(cfg),
	})
}

func (s *Server) githubIntegrationConfig(ctx context.Context) (githubIntegrationConfig, string, error) {
	if raw, ok, err := s.store.GetSetting(ctx, githubIntegrationStoreKey); err != nil {
		return githubIntegrationConfig{}, "", err
	} else if ok && strings.TrimSpace(raw) != "" {
		var cfg githubIntegrationConfig
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return githubIntegrationConfig{}, "", fmt.Errorf("stored GitHub integration config: %w", err)
		}
		cfg = sanitizeGitHubIntegration(cfg)
		if err := validateGitHubIntegration(cfg); err != nil {
			return githubIntegrationConfig{}, "", fmt.Errorf("stored GitHub integration config: %w", err)
		}
		return cfg, "store", nil
	}
	return defaultGitHubIntegration(), "default", nil
}

func defaultGitHubIntegration() githubIntegrationConfig {
	return githubIntegrationConfig{
		Enabled:      false,
		TokenEnv:     "CONSIZE_GITHUB_TOKEN",
		Repositories: []githubRepository{},
	}
}

func sanitizeGitHubIntegration(cfg githubIntegrationConfig) githubIntegrationConfig {
	cfg.Organization = strings.TrimSpace(cfg.Organization)
	cfg.TokenEnv = strings.TrimSpace(cfg.TokenEnv)
	cfg.DefaultRepo = strings.TrimSpace(cfg.DefaultRepo)
	cfg.DefaultPath = cleanRepoPath(cfg.DefaultPath)
	cfg.DefaultTerraformAddr = strings.TrimSpace(cfg.DefaultTerraformAddr)
	if cfg.Repositories == nil {
		cfg.Repositories = []githubRepository{}
	}
	for i := range cfg.Repositories {
		r := &cfg.Repositories[i]
		r.Alias = strings.TrimSpace(r.Alias)
		r.Repo = strings.TrimSpace(r.Repo)
		r.DefaultBranch = strings.TrimSpace(r.DefaultBranch)
		r.RootPath = cleanRepoPath(r.RootPath)
	}
	return cfg
}

func validateGitHubIntegration(cfg githubIntegrationConfig) error {
	if cfg.TokenEnv != "" && !envNamePattern.MatchString(cfg.TokenEnv) {
		return fmt.Errorf("token_env must be an environment variable name, for example CONSIZE_GITHUB_TOKEN")
	}
	if containsSecretLikeValue(cfg.TokenEnv) {
		return fmt.Errorf("token_env must reference an environment variable, not contain a token")
	}
	if cfg.DefaultRepo != "" && !isGitHubRepoLike(cfg.DefaultRepo) && cfg.Organization == "" && !matchesRepositoryAlias(cfg, cfg.DefaultRepo) {
		return fmt.Errorf("default_repo must be owner/repo unless organization is set")
	}
	if looksLikeURL(cfg.DefaultPath) {
		return fmt.Errorf("default_path must be a repo-relative Terraform file path, not a URL")
	}
	for i, r := range cfg.Repositories {
		if r.Repo == "" {
			return fmt.Errorf("repository %d needs a repo value such as owner/repo", i+1)
		}
		if !isGitHubRepoLike(r.Repo) && cfg.Organization == "" {
			return fmt.Errorf("repository %d must be owner/repo unless organization is set", i+1)
		}
		if looksLikeURL(r.RootPath) {
			return fmt.Errorf("repository %d root_path must be repo-relative, not a URL", i+1)
		}
	}
	return nil
}

func containsSecretLikeValue(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return strings.HasPrefix(v, "ghp_") || strings.HasPrefix(v, "github_pat_") || strings.HasPrefix(v, "gho_")
}

func githubTokenPresent(cfg githubIntegrationConfig) bool {
	if strings.TrimSpace(cfg.TokenEnv) == "" {
		return false
	}
	_, ok := os.LookupEnv(cfg.TokenEnv)
	return ok
}

func (s *Server) resolveRecommendationIaCTarget(ctx context.Context, rec store.Recommendation, wl store.Workload, repo, path, addr string) (resolvedRepo, resolvedPath, resolvedAddr string, cfg githubIntegrationConfig, err error) {
	cfg, _, err = s.githubIntegrationConfig(ctx)
	if err != nil {
		return "", "", "", githubIntegrationConfig{}, err
	}
	if !cfg.Enabled {
		return repo, path, addr, cfg, nil
	}
	selectedRepo := firstNonEmpty(repo, cfg.DefaultRepo, firstGitHubRepo(cfg))
	resolvedRepo, rootPath := resolveGitHubRepo(cfg, selectedRepo)
	resolvedRepo = firstNonEmpty(resolvedRepo, selectedRepo)
	resolvedPath = repoScopedPath(rootPath, firstNonEmpty(path, cfg.DefaultPath, defaultRecommendationIaCPath(wl)))
	resolvedAddr = firstNonEmpty(addr, cfg.DefaultTerraformAddr)
	if resolvedAddr == "" {
		resolvedAddr = defaultRecommendationTerraformAddr(rec, wl)
	}
	return resolvedRepo, resolvedPath, resolvedAddr, cfg, nil
}

func resolveGitHubRepo(cfg githubIntegrationConfig, selected string) (repo, rootPath string) {
	selected = strings.TrimSpace(selected)
	if selected == "" {
		return "", ""
	}
	for _, r := range cfg.Repositories {
		if selected == r.Alias || selected == r.Repo {
			return normalizeGitHubRepoName(cfg, r.Repo), r.RootPath
		}
	}
	return normalizeGitHubRepoName(cfg, selected), ""
}

func firstGitHubRepo(cfg githubIntegrationConfig) string {
	for _, r := range cfg.Repositories {
		if strings.TrimSpace(r.Repo) != "" {
			return strings.TrimSpace(r.Repo)
		}
	}
	return ""
}

func defaultBranchForRepo(cfg githubIntegrationConfig, selected string) string {
	selected = strings.TrimSpace(selected)
	for _, r := range cfg.Repositories {
		if selected == r.Alias || selected == r.Repo || selected == normalizeGitHubRepoName(cfg, r.Repo) {
			return strings.TrimSpace(r.DefaultBranch)
		}
	}
	return ""
}

func normalizeGitHubRepoName(cfg githubIntegrationConfig, repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" || looksLikeURL(repo) || strings.Contains(repo, "/") {
		return repo
	}
	org := strings.Trim(strings.TrimSpace(cfg.Organization), "/")
	if org == "" {
		return repo
	}
	return org + "/" + repo
}

func isGitHubRepoLike(repo string) bool {
	repo = strings.TrimSpace(repo)
	return looksLikeURL(repo) || strings.Count(repo, "/") >= 1
}

func matchesRepositoryAlias(cfg githubIntegrationConfig, value string) bool {
	value = strings.TrimSpace(value)
	for _, r := range cfg.Repositories {
		if value != "" && value == strings.TrimSpace(r.Alias) {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func cleanRepoPath(path string) string {
	return strings.TrimLeft(strings.TrimSpace(path), "/")
}

func repoScopedPath(root, path string) string {
	root = cleanRepoPath(root)
	path = cleanRepoPath(path)
	if root == "" || path == "" || strings.HasPrefix(path, root+"/") || path == root {
		return path
	}
	rootParts := strings.Split(root, "/")
	pathParts := strings.Split(path, "/")
	if len(rootParts) > 0 && len(pathParts) > 1 && rootParts[len(rootParts)-1] == pathParts[0] {
		path = strings.Join(pathParts[1:], "/")
	}
	return root + "/" + path
}
