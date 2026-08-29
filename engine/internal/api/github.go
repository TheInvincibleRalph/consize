package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"consize/internal/store"
)

func canOpenGitHubPullRequest(cfg githubIntegrationConfig, pr store.IaCPullRequest) bool {
	return cfg.Enabled &&
		githubTokenPresent(cfg) &&
		strings.TrimSpace(pr.Repo) != "" &&
		strings.TrimSpace(pr.Branch) != "" &&
		strings.TrimSpace(pr.Diff) != ""
}

func openGitHubPullRequest(ctx context.Context, cfg githubIntegrationConfig, rec store.Recommendation, wl store.Workload, pr store.IaCPullRequest) (store.IaCPullRequest, error) {
	token := strings.TrimSpace(os.Getenv(cfg.TokenEnv))
	if token == "" {
		return pr, fmt.Errorf("%s is not set", cfg.TokenEnv)
	}
	owner, repo, err := parseGitHubRepo(pr.Repo)
	if err != nil {
		return pr, err
	}
	path := diffPath(pr.Diff)
	if path == "" {
		return pr, fmt.Errorf("Terraform file path is missing")
	}
	client := githubClient{
		baseURL: strings.TrimRight(firstNonEmpty(os.Getenv("CONSIZE_GITHUB_API_BASE"), "https://api.github.com"), "/"),
		token:   token,
		http:    http.DefaultClient,
	}
	defaultBranch := defaultBranchForRepo(cfg, pr.Repo)
	if defaultBranch == "" {
		defaultBranch, err = client.defaultBranch(ctx, owner, repo)
		if err != nil {
			return pr, err
		}
	}
	baseSHA, err := client.branchSHA(ctx, owner, repo, defaultBranch)
	if err != nil {
		return pr, err
	}
	content, sha, err := client.getFile(ctx, owner, repo, path, defaultBranch)
	if err != nil {
		return pr, err
	}
	kind, name, ok := terraformResourceFromDiff(pr.Diff)
	if !ok {
		return pr, fmt.Errorf("Terraform resource could not be resolved from generated diff")
	}
	next, err := patchRecommendationTerraformContent(content, rec, wl, kind, name)
	if err != nil {
		return pr, err
	}
	branch := pr.Branch
	if err := client.createBranch(ctx, owner, repo, branch, baseSHA); err != nil {
		if isGitHubConflict(err) {
			branch = branch + "-" + time.Now().UTC().Format("20060102150405")
			if err := client.createBranch(ctx, owner, repo, branch, baseSHA); err != nil {
				return pr, err
			}
		} else {
			return pr, err
		}
	}
	message := fmt.Sprintf("Rightsize %s/%s %s", wl.Namespace, wl.Name, rec.Resource)
	if err := client.updateFile(ctx, owner, repo, path, branch, message, next, sha); err != nil {
		return pr, err
	}
	url, err := client.createPullRequest(ctx, owner, repo, pr.Title, pr.Body, branch, defaultBranch)
	if err != nil {
		return pr, err
	}
	pr.Branch = branch
	pr.Status = store.IaCPROpened
	pr.URL = url
	pr.Error = ""
	return pr, nil
}

func openGitHubPullRequestForCostOpportunity(ctx context.Context, cfg githubIntegrationConfig, opp store.CostOpportunity, pr store.IaCPullRequest) (store.IaCPullRequest, error) {
	token := strings.TrimSpace(os.Getenv(cfg.TokenEnv))
	if token == "" {
		return pr, fmt.Errorf("%s is not set", cfg.TokenEnv)
	}
	owner, repo, err := parseGitHubRepo(pr.Repo)
	if err != nil {
		return pr, err
	}
	path := diffPath(pr.Diff)
	if path == "" {
		return pr, fmt.Errorf("Terraform file path is missing")
	}
	client := githubClient{
		baseURL: strings.TrimRight(firstNonEmpty(os.Getenv("CONSIZE_GITHUB_API_BASE"), "https://api.github.com"), "/"),
		token:   token,
		http:    http.DefaultClient,
	}
	defaultBranch := defaultBranchForRepo(cfg, pr.Repo)
	if defaultBranch == "" {
		defaultBranch, err = client.defaultBranch(ctx, owner, repo)
		if err != nil {
			return pr, err
		}
	}
	baseSHA, err := client.branchSHA(ctx, owner, repo, defaultBranch)
	if err != nil {
		return pr, err
	}
	content, sha, err := client.getFile(ctx, owner, repo, path, defaultBranch)
	if err != nil {
		return pr, err
	}
	kind, name, ok := terraformResourceFromDiff(pr.Diff)
	if !ok {
		return pr, fmt.Errorf("Terraform resource could not be resolved from generated diff")
	}
	next, err := patchCostOpportunityTerraformContent(content, opp, kind, name)
	if err != nil {
		return pr, err
	}
	branch := pr.Branch
	if err := client.createBranch(ctx, owner, repo, branch, baseSHA); err != nil {
		if isGitHubConflict(err) {
			branch = branch + "-" + time.Now().UTC().Format("20060102150405")
			if err := client.createBranch(ctx, owner, repo, branch, baseSHA); err != nil {
				return pr, err
			}
		} else {
			return pr, err
		}
	}
	message := fmt.Sprintf("Remove %s %s", humanResourceType(opp.ResourceType), displayName(opp))
	if err := client.updateFile(ctx, owner, repo, path, branch, message, next, sha); err != nil {
		return pr, err
	}
	url, err := client.createPullRequest(ctx, owner, repo, pr.Title, pr.Body, branch, defaultBranch)
	if err != nil {
		return pr, err
	}
	pr.Branch = branch
	pr.Status = store.IaCPROpened
	pr.URL = url
	pr.Error = ""
	return pr, nil
}

type githubClient struct {
	baseURL string
	token   string
	http    *http.Client
}

type githubError struct {
	Status int
	Body   string
}

func (e githubError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("github: HTTP %d", e.Status)
	}
	return fmt.Sprintf("github: HTTP %d: %s", e.Status, e.Body)
}

func isGitHubConflict(err error) bool {
	if e, ok := err.(githubError); ok {
		return e.Status == http.StatusConflict || e.Status == http.StatusUnprocessableEntity
	}
	return false
}

func (c githubClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", firstNonEmpty(os.Getenv("CONSIZE_GITHUB_API_VERSION"), "2022-11-28"))
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return githubError{Status: res.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode github response: %w", err)
	}
	return nil
}

func (c githubClient) defaultBranch(ctx context.Context, owner, repo string) (string, error) {
	var body struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo)), nil, &body); err != nil {
		return "", err
	}
	if strings.TrimSpace(body.DefaultBranch) == "" {
		return "", fmt.Errorf("github repository default branch is empty")
	}
	return body.DefaultBranch, nil
}

func (c githubClient) branchSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	var body struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	path := fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch))
	if err := c.do(ctx, http.MethodGet, path, nil, &body); err != nil {
		return "", err
	}
	if strings.TrimSpace(body.Object.SHA) == "" {
		return "", fmt.Errorf("github branch %q has no commit sha", branch)
	}
	return body.Object.SHA, nil
}

func (c githubClient) createBranch(ctx context.Context, owner, repo, branch, sha string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/git/refs", url.PathEscape(owner), url.PathEscape(repo)), map[string]string{
		"ref": "refs/heads/" + branch,
		"sha": sha,
	}, nil)
}

func (c githubClient) getFile(ctx context.Context, owner, repo, path, ref string) (content string, sha string, err error) {
	var raw json.RawMessage
	p := fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", url.PathEscape(owner), url.PathEscape(repo), escapeRepoPath(path), url.QueryEscape(ref))
	if err := c.do(ctx, http.MethodGet, p, nil, &raw); err != nil {
		return "", "", err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return "", "", fmt.Errorf("GitHub path %q is a directory; select a Terraform file such as %s/workloads.tf", path, strings.TrimRight(path, "/"))
	}
	var body struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		SHA      string `json:"sha"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", "", fmt.Errorf("decode github file %s response: %w", path, err)
	}
	if body.Encoding != "base64" {
		return "", "", fmt.Errorf("github file %s returned unsupported encoding %q", path, body.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body.Content, "\n", ""))
	if err != nil {
		return "", "", fmt.Errorf("decode github file %s: %w", path, err)
	}
	return string(decoded), body.SHA, nil
}

func (c githubClient) updateFile(ctx context.Context, owner, repo, path, branch, message, content, sha string) error {
	payload := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"sha":     sha,
		"branch":  branch,
	}
	p := fmt.Sprintf("/repos/%s/%s/contents/%s", url.PathEscape(owner), url.PathEscape(repo), escapeRepoPath(path))
	return c.do(ctx, http.MethodPut, p, payload, nil)
}

func (c githubClient) createPullRequest(ctx context.Context, owner, repo, title, body, head, base string) (string, error) {
	var res struct {
		HTMLURL string `json:"html_url"`
		URL     string `json:"url"`
	}
	payload := map[string]any{
		"title": title,
		"body":  body,
		"head":  head,
		"base":  base,
		"draft": true,
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(owner), url.PathEscape(repo)), payload, &res); err != nil {
		return "", err
	}
	return firstNonEmpty(res.HTMLURL, res.URL), nil
}

func parseGitHubRepo(raw string) (owner, repo string, err error) {
	raw = strings.TrimSpace(strings.TrimSuffix(raw, ".git"))
	raw = strings.TrimPrefix(raw, "https://github.com/")
	raw = strings.TrimPrefix(raw, "http://github.com/")
	raw = strings.TrimPrefix(raw, "git@github.com:")
	raw = strings.Trim(raw, "/")
	parts := strings.Split(raw, "/")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("repo must be owner/repo or a github.com URL")
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}

func escapeRepoPath(path string) string {
	parts := strings.Split(cleanRepoPath(path), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func diffPath(diff string) string {
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git a/") {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				return strings.TrimPrefix(fields[2], "a/")
			}
		}
	}
	return ""
}

func patchRecommendationTerraformContent(content string, rec store.Recommendation, wl store.Workload, kind, name string) (string, error) {
	start, end := terraformResourceBlock(content, kind, name)
	if start < 0 || end <= start {
		return "", fmt.Errorf("could not find Terraform resource %q %q in the selected file; choose the file that owns this workload or update the Terraform resource field", kind, name)
	}
	block := content[start:end]
	replacements := [][2]string{}
	switch rec.Resource {
	case store.ResourceClass:
		attr := "instance_class"
		if wl.DBProvider == "gcp" {
			attr = "settings.tier"
		}
		replacements = append(replacements, [2]string{
			fmt.Sprintf(`%s = %q`, attr, rec.ClassCurrent),
			fmt.Sprintf(`%s = %q`, attr, rec.ClassProposed),
		})
	case store.ResourceMemory:
		replacements = append(replacements,
			[2]string{fmt.Sprintf(`requests = { memory = %q }`, mib(rec.CurrentValue)), fmt.Sprintf(`requests = { memory = %q }`, mib(rec.ProposedValue))},
			[2]string{fmt.Sprintf(`limits   = { memory = %q }`, mib(rec.CurrentLimit)), fmt.Sprintf(`limits   = { memory = %q }`, mib(rec.ProposedLimit))},
		)
	default:
		replacements = append(replacements,
			[2]string{fmt.Sprintf(`requests = { cpu = %q }`, milli(rec.CurrentValue)), fmt.Sprintf(`requests = { cpu = %q }`, milli(rec.ProposedValue))},
			[2]string{fmt.Sprintf(`limits   = { cpu = %q }`, milli(rec.CurrentLimit)), fmt.Sprintf(`limits   = { cpu = %q }`, milli(rec.ProposedLimit))},
		)
	}
	nextBlock := block
	for _, pair := range replacements {
		if !strings.Contains(nextBlock, pair[0]) {
			return "", fmt.Errorf("could not find exact Terraform value %q for %s/%s; prepare the plan and adjust the mapping", pair[0], wl.Namespace, wl.Name)
		}
		nextBlock = strings.Replace(nextBlock, pair[0], pair[1], 1)
	}
	return content[:start] + nextBlock + content[end:], nil
}

func patchCostOpportunityTerraformContent(content string, opp store.CostOpportunity, kind, name string) (string, error) {
	start, end := terraformResourceBlock(content, kind, name)
	if start < 0 || end <= start {
		return "", fmt.Errorf("could not find Terraform resource %q %q in the selected file; choose the file that owns this resource or update the Terraform resource field", kind, name)
	}
	comment := fmt.Sprintf(`# Removed by Consize recommendation:
# %s
# Estimated savings: $%.2f/mo`, opp.Recommendation, opp.MonthlyCost)
	return content[:start] + comment + "\n" + content[end:], nil
}

func terraformResourceFromDiff(diff string) (kind, name string, ok bool) {
	for _, line := range strings.Split(diff, "\n") {
		line = strings.TrimSpace(line)
		if len(line) > 1 && (line[0] == '-' || line[0] == '+' || line[0] == ' ') {
			line = strings.TrimSpace(line[1:])
		}
		if !strings.HasPrefix(line, "resource ") {
			continue
		}
		parts := quotedStrings(line)
		if len(parts) >= 2 {
			return parts[0], parts[1], true
		}
	}
	return "", "", false
}

func terraformResourceBlock(content, kind, name string) (start, end int) {
	needle := fmt.Sprintf(`resource "%s" "%s"`, kind, name)
	start = strings.Index(content, needle)
	if start < 0 {
		return -1, -1
	}
	open := strings.Index(content[start:], "{")
	if open < 0 {
		return -1, -1
	}
	pos := start + open
	depth := 0
	inString := false
	escaped := false
	for i := pos; i < len(content); i++ {
		ch := content[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			continue
		}
		if ch == '{' {
			depth++
			continue
		}
		if ch == '}' {
			depth--
			if depth == 0 {
				return start, i + 1
			}
		}
	}
	return -1, -1
}

func quotedStrings(line string) []string {
	var out []string
	in := false
	escaped := false
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if !in {
			if ch == '"' {
				in = true
				b.Reset()
			}
			continue
		}
		if escaped {
			b.WriteByte(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			out = append(out, b.String())
			in = false
			continue
		}
		b.WriteByte(ch)
	}
	return out
}
