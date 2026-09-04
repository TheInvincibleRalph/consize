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

	"consize/internal/store"
	yaml "go.yaml.in/yaml/v3"
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
		return pr, fmt.Errorf("source file path is missing")
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
	var next string
	switch iacProviderForPath(path) {
	case "kubernetes-yaml":
		next, err = patchRecommendationKubernetesYAMLContent(content, rec, wl)
	case "helm-values":
		err = fmt.Errorf("Helm values PRs need an explicit values key mapping; use a Kubernetes YAML manifest or Terraform file for this PR, or add Helm values mapping first")
	default:
		var kind, name string
		var ok bool
		kind, name, ok = terraformResourceFromDiff(pr.Diff)
		if !ok {
			return pr, fmt.Errorf("Terraform resource could not be resolved from generated diff")
		}
		next, err = patchRecommendationTerraformContent(content, rec, wl, kind, name)
	}
	if err != nil {
		return pr, err
	}
	branch := pr.Branch
	if err := client.createBranch(ctx, owner, repo, branch, baseSHA); err != nil {
		if isGitHubConflict(err) {
			existingURL, ok, lookupErr := client.findOpenPullRequest(ctx, owner, repo, branch, defaultBranch)
			if lookupErr != nil {
				return pr, lookupErr
			}
			if ok {
				pr.Status = store.IaCPROpened
				pr.URL = existingURL
				pr.Error = ""
				return pr, nil
			}
			return pr, fmt.Errorf("GitHub branch %q already exists without an open PR; close, rename, or delete that branch before opening a new Consize PR", branch)
		}
		return pr, err
	}
	message := fmt.Sprintf("Rightsize %s/%s %s", wl.Namespace, wl.Name, rec.Resource)
	if err := client.updateFile(ctx, owner, repo, path, branch, message, next, sha); err != nil {
		return pr, err
	}
	url, err := client.createPullRequest(ctx, owner, repo, pr.Title, pr.Body, branch, defaultBranch)
	if err != nil {
		if isGitHubConflict(err) {
			existingURL, ok, lookupErr := client.findOpenPullRequest(ctx, owner, repo, branch, defaultBranch)
			if lookupErr != nil {
				return pr, lookupErr
			}
			if ok {
				pr.Status = store.IaCPROpened
				pr.URL = existingURL
				pr.Error = ""
				return pr, nil
			}
		}
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
		return pr, fmt.Errorf("source file path is missing")
	}
	if !isTerraformFilePath(path) {
		return pr, fmt.Errorf("cloud waste PRs currently require a Terraform file because the resources are cloud-provider objects, not Kubernetes workload manifests")
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
			existingURL, ok, lookupErr := client.findOpenPullRequest(ctx, owner, repo, branch, defaultBranch)
			if lookupErr != nil {
				return pr, lookupErr
			}
			if ok {
				pr.Status = store.IaCPROpened
				pr.URL = existingURL
				pr.Error = ""
				return pr, nil
			}
			return pr, fmt.Errorf("GitHub branch %q already exists without an open PR; close, rename, or delete that branch before opening a new Consize PR", branch)
		}
		return pr, err
	}
	message := fmt.Sprintf("Remove %s %s", humanResourceType(opp.ResourceType), displayName(opp))
	if err := client.updateFile(ctx, owner, repo, path, branch, message, next, sha); err != nil {
		return pr, err
	}
	url, err := client.createPullRequest(ctx, owner, repo, pr.Title, pr.Body, branch, defaultBranch)
	if err != nil {
		if isGitHubConflict(err) {
			existingURL, ok, lookupErr := client.findOpenPullRequest(ctx, owner, repo, branch, defaultBranch)
			if lookupErr != nil {
				return pr, lookupErr
			}
			if ok {
				pr.Status = store.IaCPROpened
				pr.URL = existingURL
				pr.Error = ""
				return pr, nil
			}
		}
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
		return "", "", fmt.Errorf("GitHub path %q is a directory; select a source file such as %s/workloads.tf or %s/deployment.yaml", path, strings.TrimRight(path, "/"), strings.TrimRight(path, "/"))
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

func (c githubClient) findOpenPullRequest(ctx context.Context, owner, repo, head, base string) (string, bool, error) {
	var prs []struct {
		HTMLURL string `json:"html_url"`
		URL     string `json:"url"`
	}
	path := fmt.Sprintf(
		"/repos/%s/%s/pulls?state=open&head=%s&base=%s",
		url.PathEscape(owner),
		url.PathEscape(repo),
		url.QueryEscape(owner+":"+head),
		url.QueryEscape(base),
	)
	if err := c.do(ctx, http.MethodGet, path, nil, &prs); err != nil {
		return "", false, err
	}
	for _, pr := range prs {
		if link := firstNonEmpty(pr.HTMLURL, pr.URL); link != "" {
			return link, true, nil
		}
	}
	return "", false, nil
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

func patchRecommendationKubernetesYAMLContent(content string, rec store.Recommendation, wl store.Workload) (string, error) {
	if rec.Resource == store.ResourceClass {
		return "", fmt.Errorf("database class recommendations need Terraform or provider-specific IaC, not a Kubernetes YAML manifest")
	}
	docs, err := decodeYAMLDocuments(content)
	if err != nil {
		return "", err
	}
	if len(docs) == 0 {
		return "", fmt.Errorf("YAML file is empty")
	}
	var matched bool
	for i := range docs {
		root := yamlDocumentRoot(&docs[i])
		if root == nil || !kubernetesDocumentMatches(root, wl) {
			continue
		}
		matched = true
		container, err := selectWorkloadContainer(root, rec, wl)
		if err != nil {
			return "", err
		}
		return patchKubernetesYAMLResourceText(content, rec, wl, container.Line)
	}
	if !matched {
		return "", fmt.Errorf("could not find Kubernetes %s %q for namespace %q in the selected YAML file", kubernetesManifestKind(wl), wl.Name, wl.Namespace)
	}
	return "", fmt.Errorf("could not patch Kubernetes %s %q", kubernetesManifestKind(wl), wl.Name)
}

func patchKubernetesYAMLResourceText(content string, rec store.Recommendation, wl store.Workload, containerLine int) (string, error) {
	lines := splitLinesPreservingEndings(content)
	containerIndex := containerLine - 1
	if containerIndex < 0 || containerIndex >= len(lines) {
		return "", fmt.Errorf("could not locate container line for Kubernetes %s %q", kubernetesManifestKind(wl), wl.Name)
	}
	containerIndent := leadingSpaces(lineBody(lines[containerIndex]))
	containerEnd := len(lines)
	for i := containerIndex + 1; i < len(lines); i++ {
		body := lineBody(lines[i])
		trimmed := strings.TrimSpace(body)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "---") {
			containerEnd = i
			break
		}
		indent := leadingSpaces(body)
		if indent <= containerIndent {
			containerEnd = i
			break
		}
	}

	resourcesIndex := -1
	resourcesIndent := -1
	for i := containerIndex + 1; i < containerEnd; i++ {
		body := lineBody(lines[i])
		if strings.TrimSpace(body) == "resources:" {
			resourcesIndex = i
			resourcesIndent = leadingSpaces(body)
			break
		}
	}
	if resourcesIndex < 0 {
		return "", fmt.Errorf("could not find resources for Kubernetes %s %q; add requests/limits before opening a PR", kubernetesManifestKind(wl), wl.Name)
	}

	var field, current, proposed, currentLimit, proposedLimit string
	if rec.Resource == store.ResourceMemory {
		field, current, proposed = "memory", mib(rec.CurrentValue), mib(rec.ProposedValue)
		currentLimit, proposedLimit = mib(rec.CurrentLimit), mib(rec.ProposedLimit)
	} else {
		field, current, proposed = "cpu", milli(rec.CurrentValue), milli(rec.ProposedValue)
		currentLimit, proposedLimit = milli(rec.CurrentLimit), milli(rec.ProposedLimit)
	}
	if err := patchKubernetesResourceGroup(lines, resourcesIndex, containerEnd, resourcesIndent, "requests", field, current, proposed, wl); err != nil {
		return "", err
	}
	if err := patchKubernetesResourceGroup(lines, resourcesIndex, containerEnd, resourcesIndent, "limits", field, currentLimit, proposedLimit, wl); err != nil {
		return "", err
	}
	return strings.Join(lines, ""), nil
}

func patchKubernetesResourceGroup(lines []string, resourcesIndex, containerEnd, resourcesIndent int, group, field, current, proposed string, wl store.Workload) error {
	groupIndex := -1
	groupIndent := -1
	for i := resourcesIndex + 1; i < containerEnd; i++ {
		body := lineBody(lines[i])
		trimmed := strings.TrimSpace(body)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := leadingSpaces(body)
		if indent <= resourcesIndent {
			break
		}
		if trimmed == group+":" {
			groupIndex = i
			groupIndent = indent
			break
		}
	}
	if groupIndex < 0 {
		return fmt.Errorf("could not find resources.%s for %s/%s; add the field before opening a PR", group, wl.Namespace, wl.Name)
	}
	for i := groupIndex + 1; i < containerEnd; i++ {
		body := lineBody(lines[i])
		trimmed := strings.TrimSpace(body)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := leadingSpaces(body)
		if indent <= groupIndent {
			break
		}
		if strings.HasPrefix(trimmed, field+":") {
			next, err := replaceYAMLScalarLine(lines[i], field, current, proposed)
			if err != nil {
				return fmt.Errorf("refusing to patch resources.%s.%s for %s/%s: %w", group, field, wl.Namespace, wl.Name, err)
			}
			lines[i] = next
			return nil
		}
	}
	return fmt.Errorf("could not find resources.%s.%s for %s/%s; add the field before opening a PR", group, field, wl.Namespace, wl.Name)
}

func replaceYAMLScalarLine(line, field, current, proposed string) (string, error) {
	ending := lineEnding(line)
	body := lineBody(line)
	indent := body[:leadingSpaces(body)]
	trimmed := strings.TrimSpace(body)
	if !strings.HasPrefix(trimmed, field+":") {
		return "", fmt.Errorf("line does not contain %s", field)
	}
	rawValue := strings.TrimSpace(strings.TrimPrefix(trimmed, field+":"))
	comment := ""
	if value, suffix, ok := strings.Cut(rawValue, "#"); ok {
		rawValue = strings.TrimSpace(value)
		comment = " #" + suffix
	}
	quote := ""
	value := rawValue
	if len(value) >= 2 {
		first, last := value[:1], value[len(value)-1:]
		if (first == `"` && last == `"`) || (first == `'` && last == `'`) {
			quote = first
			value = value[1 : len(value)-1]
		}
	}
	if value != current {
		return "", fmt.Errorf("repo has %q but Consize expected %q", value, current)
	}
	return fmt.Sprintf("%s%s: %s%s%s%s", indent, field, quote, proposed, quote, comment) + ending, nil
}

func splitLinesPreservingEndings(s string) []string {
	if s == "" {
		return []string{}
	}
	lines := []string{}
	start := 0
	for i, r := range s {
		if r == '\n' {
			lines = append(lines, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func lineBody(line string) string {
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
}

func lineEnding(line string) string {
	if strings.HasSuffix(line, "\r\n") {
		return "\r\n"
	}
	if strings.HasSuffix(line, "\n") {
		return "\n"
	}
	return ""
}

func leadingSpaces(s string) int {
	return len(s) - len(strings.TrimLeft(s, " "))
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

func decodeYAMLDocuments(content string) ([]yaml.Node, error) {
	dec := yaml.NewDecoder(strings.NewReader(content))
	var docs []yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse Kubernetes YAML: %w", err)
		}
		if yamlDocumentRoot(&doc) == nil {
			continue
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

func encodeYAMLDocuments(docs []yaml.Node) (string, error) {
	parts := make([]string, 0, len(docs))
	for i := range docs {
		raw, err := yaml.Marshal(&docs[i])
		if err != nil {
			return "", fmt.Errorf("render Kubernetes YAML: %w", err)
		}
		parts = append(parts, strings.TrimSpace(string(raw)))
	}
	return strings.Join(parts, "\n---\n") + "\n", nil
}

func yamlDocumentRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	if doc.Kind == yaml.MappingNode {
		return doc
	}
	return nil
}

func kubernetesDocumentMatches(root *yaml.Node, wl store.Workload) bool {
	kind := strings.TrimSpace(mappingString(root, "kind"))
	if !strings.EqualFold(kind, kubernetesManifestKind(wl)) {
		return false
	}
	meta := mappingValue(root, "metadata")
	if meta == nil {
		return false
	}
	if mappingString(meta, "name") != wl.Name {
		return false
	}
	ns := strings.TrimSpace(mappingString(meta, "namespace"))
	return ns == "" || wl.Namespace == "" || ns == wl.Namespace
}

func selectWorkloadContainer(root *yaml.Node, rec store.Recommendation, wl store.Workload) (*yaml.Node, error) {
	containers := containersNode(root, wl)
	if containers == nil || containers.Kind != yaml.SequenceNode || len(containers.Content) == 0 {
		return nil, fmt.Errorf("could not find containers for Kubernetes %s %q in the selected YAML file", kubernetesManifestKind(wl), wl.Name)
	}
	targetName := strings.TrimSpace(firstNonEmpty(wl.Labels["consize.dev/container"], wl.Labels["consize.io/container"], wl.Labels["container"]))
	if targetName != "" {
		for _, c := range containers.Content {
			if mappingString(c, "name") == targetName {
				return c, nil
			}
		}
		return nil, fmt.Errorf("could not find container %q in Kubernetes %s %q", targetName, kubernetesManifestKind(wl), wl.Name)
	}
	for _, c := range containers.Content {
		if mappingString(c, "name") == wl.Name || containerHasCurrentRecommendation(c, rec) {
			return c, nil
		}
	}
	if len(containers.Content) == 1 {
		return containers.Content[0], nil
	}
	return nil, fmt.Errorf("the selected manifest has multiple containers; add a consize.dev/container label or use a manifest with one container so Consize can patch the right one")
}

func containersNode(root *yaml.Node, wl store.Workload) *yaml.Node {
	if strings.EqualFold(kubernetesManifestKind(wl), "CronJob") {
		return mappingAtPath(root, []string{"spec", "jobTemplate", "spec", "template", "spec", "containers"})
	}
	return mappingAtPath(root, []string{"spec", "template", "spec", "containers"})
}

func containerHasCurrentRecommendation(container *yaml.Node, rec store.Recommendation) bool {
	resources := mappingValue(container, "resources")
	if resources == nil {
		return false
	}
	field := "cpu"
	current := milli(rec.CurrentValue)
	currentLimit := milli(rec.CurrentLimit)
	if rec.Resource == store.ResourceMemory {
		field = "memory"
		current = mib(rec.CurrentValue)
		currentLimit = mib(rec.CurrentLimit)
	}
	req := mappingAtPath(resources, []string{"requests", field})
	lim := mappingAtPath(resources, []string{"limits", field})
	return (req != nil && req.Value == current) || (lim != nil && lim.Value == currentLimit)
}

func setResourceQuantity(resources *yaml.Node, group, field, current, proposed string, wl store.Workload) error {
	if proposed == "" || proposed == "0m" || proposed == "0Mi" {
		return nil
	}
	groupNode := ensureMappingAtPath(resources, []string{group})
	valueNode := mappingValue(groupNode, field)
	if valueNode != nil {
		if valueNode.Value != "" && current != "" && valueNode.Value != current {
			return fmt.Errorf("refusing to patch %s.%s for %s/%s because the repo has %q but Consize expected %q", group, field, wl.Namespace, wl.Name, valueNode.Value, current)
		}
		valueNode.Kind = yaml.ScalarNode
		valueNode.Tag = "!!str"
		valueNode.Value = proposed
		return nil
	}
	groupNode.Content = append(groupNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: field}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: proposed})
	return nil
}

func mappingAtPath(root *yaml.Node, path []string) *yaml.Node {
	current := root
	for _, key := range path {
		current = mappingValue(current, key)
		if current == nil {
			return nil
		}
	}
	return current
}

func ensureMappingAtPath(root *yaml.Node, path []string) *yaml.Node {
	current := root
	for _, key := range path {
		next := mappingValue(current, key)
		if next == nil {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			current.Content = append(current.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, next)
		}
		if next.Kind != yaml.MappingNode {
			next.Kind = yaml.MappingNode
			next.Tag = "!!map"
			next.Value = ""
			next.Content = nil
		}
		current = next
	}
	return current
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func mappingString(node *yaml.Node, key string) string {
	child := mappingValue(node, key)
	if child == nil {
		return ""
	}
	return strings.TrimSpace(child.Value)
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
