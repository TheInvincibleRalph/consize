package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"consize/internal/auth"
	"consize/internal/config"
	"consize/internal/costscan"
	"consize/internal/store"
)

func (s *Server) listCostOpportunities(w http.ResponseWriter, r *http.Request) {
	opps, err := s.store.ListCostOpportunities(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, err)
		return
	}
	actions, err := s.store.ListCostActions(r.Context(), nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	prs, err := s.store.ListIaCPullRequests(r.Context(), nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	latest := map[int64]store.IaCPullRequest{}
	for _, pr := range prs {
		if _, ok := latest[pr.OpportunityID]; !ok {
			latest[pr.OpportunityID] = pr
		}
	}
	var total float64
	for _, o := range opps {
		if o.Status == store.OpportunityOpen || o.Status == store.OpportunityPRReady {
			total += o.MonthlyCost
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"opportunities": opps,
		"actions":       actions,
		"latest_prs":    latest,
		"summary": map[string]any{
			"count":           len(opps),
			"monthly_savings": total,
		},
	})
}

func (s *Server) scanCostOpportunities(w http.ResponseWriter, r *http.Request) {
	src, via, err := costscan.SourceFor(config.Str("CONSIZE_COSTSCAN", "none"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	opps, err := costscan.Service{Source: src, Store: s.store}.Run(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	var total float64
	for _, o := range opps {
		total += o.MonthlyCost
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"opportunities": opps,
		"summary": map[string]any{
			"count":           len(opps),
			"monthly_savings": total,
		},
		"source": via,
	})
}

func (s *Server) applyCostOpportunity(w http.ResponseWriter, r *http.Request) {
	id, err := parseCostOpportunityID(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cost opportunity id must be a positive integer"})
		return
	}
	var body struct {
		Mode  string `json:"mode"`
		Actor string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"mode\": \"dry_run|approved\", \"actor\": \"...\"}"})
		return
	}
	opp, err := s.store.GetCostOpportunity(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "cost opportunity not found"})
			return
		}
		writeErr(w, err)
		return
	}
	if opp.Status != store.OpportunityOpen && opp.Status != store.OpportunityPRReady {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "cost opportunity is not open"})
		return
	}
	actor := strings.TrimSpace(body.Actor)
	if s.authSvc != nil {
		u, ok := auth.UserFromContext(r.Context())
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		actor = "api:" + u.Email
	}
	if actor == "" {
		actor = "operator"
	}
	mode := strings.TrimSpace(strings.ToLower(body.Mode))
	if mode == "" {
		mode = "dry_run"
	}
	if _, err := s.store.CreateCostAction(r.Context(), store.CostAction{
		OpportunityID: opp.ID,
		Actor:         actor,
		Mode:          mode,
		Result:        store.CostActionRequested,
		Message:       fmt.Sprintf("Requested direct cleanup for %s.", displayName(opp)),
		Evidence: map[string]any{
			"provider":      opp.Provider,
			"account":       opp.Account,
			"region":        opp.Region,
			"resource_type": opp.ResourceType,
			"resource_id":   opp.ResourceID,
			"action":        opp.Action,
		},
	}); err != nil {
		writeErr(w, err)
		return
	}
	applier, err := costscan.DirectApplierFor(opp.Provider)
	if err != nil {
		_, _ = s.store.CreateCostAction(r.Context(), store.CostAction{
			OpportunityID: opp.ID,
			Actor:         actor,
			Mode:          mode,
			Result:        store.CostActionFailed,
			Message:       err.Error(),
			Evidence: map[string]any{
				"provider":      opp.Provider,
				"resource_type": opp.ResourceType,
				"resource_id":   opp.ResourceID,
			},
		})
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	res, err := applier.ApplyDirect(r.Context(), opp, mode)
	if err != nil {
		_, _ = s.store.CreateCostAction(r.Context(), store.CostAction{
			OpportunityID: opp.ID,
			Actor:         actor,
			Mode:          mode,
			Result:        store.CostActionFailed,
			Message:       err.Error(),
			Evidence: map[string]any{
				"provider":      opp.Provider,
				"resource_type": opp.ResourceType,
				"resource_id":   opp.ResourceID,
			},
		})
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	result := store.CostActionDryRun
	if res.Applied {
		result = store.CostActionApplied
	}
	if _, err := s.store.CreateCostAction(r.Context(), store.CostAction{
		OpportunityID: opp.ID,
		Actor:         actor,
		Mode:          mode,
		Result:        result,
		Message:       res.Message,
		Evidence: map[string]any{
			"provider":      res.Provider,
			"resource_type": res.ResourceType,
			"resource_id":   res.ResourceID,
			"name":          res.Name,
			"applied":       res.Applied,
		},
	}); err != nil {
		writeErr(w, err)
		return
	}
	if res.Applied {
		if err := s.store.SetCostOpportunityStatus(r.Context(), id, store.OpportunityResolved); err != nil {
			writeErr(w, err)
			return
		}
		opp.Status = store.OpportunityResolved
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res, "opportunity": opp, "actor": actor})
}

func (s *Server) prepareIaCPullRequest(w http.ResponseWriter, r *http.Request) {
	id, err := parseCostOpportunityID(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cost opportunity id must be a positive integer"})
		return
	}
	var body struct {
		Repo  string `json:"repo"`
		Actor string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"repo\": \"optional repo\"}"})
		return
	}
	opp, err := s.store.GetCostOpportunity(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "cost opportunity not found"})
			return
		}
		writeErr(w, err)
		return
	}
	actor := strings.TrimSpace(body.Actor)
	if s.authSvc != nil {
		u, ok := auth.UserFromContext(r.Context())
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		actor = "api:" + u.Email
	}
	if actor == "" {
		actor = "operator"
	}
	repo, path, cfg, err := s.resolveCostOpportunityIaCTarget(r.Context(), opp, strings.TrimSpace(body.Repo))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := validateTerraformFilePath(path); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	pr := buildIaCPlan(opp, repo, path, actor)
	if canOpenGitHubPullRequest(cfg, pr) {
		opened, err := openGitHubPullRequestForCostOpportunity(r.Context(), cfg, opp, pr)
		if err != nil {
			pr.Status = store.IaCPRFailed
			pr.Error = err.Error()
		} else {
			pr = opened
		}
	}
	created, err := s.store.CreateIaCPullRequest(r.Context(), pr)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "cost opportunity not found"})
			return
		}
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pull_request": created, "opportunity": opp})
}

func (s *Server) prepareRecommendationIaCPullRequest(w http.ResponseWriter, r *http.Request) {
	id, err := parsePositiveInt64(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "recommendation id must be a positive integer"})
		return
	}
	var body struct {
		Repo          string `json:"repo"`
		Path          string `json:"path"`
		TerraformAddr string `json:"terraform_addr"`
		Actor         string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"repo\": \"optional repo\", \"path\": \"optional path\"}"})
		return
	}
	rec, err := s.store.GetRecommendation(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "recommendation not found"})
			return
		}
		writeErr(w, err)
		return
	}
	wl, err := s.store.GetWorkload(r.Context(), rec.WorkloadID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "workload not found"})
			return
		}
		writeErr(w, err)
		return
	}
	actor := strings.TrimSpace(body.Actor)
	if s.authSvc != nil {
		u, ok := auth.UserFromContext(r.Context())
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		actor = "api:" + u.Email
	}
	if actor == "" {
		actor = "operator"
	}
	repo, path, addr := normalizeRecommendationIaCInput(body.Repo, body.Path, body.TerraformAddr)
	repo, path, addr, cfg, err := s.resolveRecommendationIaCTarget(r.Context(), rec, wl, repo, path, addr)
	if err != nil {
		writeErr(w, err)
		return
	}
	if path == "" {
		path = defaultRecommendationIaCPath(wl)
	}
	if err := validateIaCFilePath(path); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	pr := buildRecommendationIaCPlan(rec, wl, repo, path, addr, actor)
	if canOpenGitHubPullRequest(cfg, pr) {
		opened, err := openGitHubPullRequest(r.Context(), cfg, rec, wl, pr)
		if err != nil {
			pr.Status = store.IaCPRFailed
			pr.Error = err.Error()
		} else {
			pr = opened
		}
	}
	created, err := s.store.CreateIaCPullRequest(r.Context(), pr)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "recommendation not found"})
			return
		}
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pull_request": created, "recommendation": rec, "workload": wl})
}

func parseCostOpportunityID(r *http.Request) (int64, error) {
	return parsePositiveInt64(chi.URLParam(r, "id"))
}

func parsePositiveInt64(raw string) (int64, error) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid id")
	}
	return n, nil
}

var branchSafe = regexp.MustCompile(`[^a-z0-9._-]+`)
var terraformNameSafe = regexp.MustCompile(`[^a-z0-9_]+`)

func (s *Server) resolveCostOpportunityIaCTarget(ctx context.Context, opp store.CostOpportunity, repo string) (resolvedRepo, resolvedPath string, cfg githubIntegrationConfig, err error) {
	cfg, _, err = s.githubIntegrationConfig(ctx)
	if err != nil {
		return "", "", githubIntegrationConfig{}, err
	}
	if !cfg.Enabled {
		return firstNonEmpty(repo, opp.IaCRepo), firstNonEmpty(opp.IaCPath, "terraform/main.tf"), cfg, nil
	}
	selectedRepo := firstNonEmpty(repo, opp.IaCRepo, cfg.DefaultRepo, firstGitHubRepo(cfg))
	resolvedRepo, rootPath := resolveGitHubRepo(cfg, selectedRepo)
	resolvedRepo = firstNonEmpty(resolvedRepo, selectedRepo)
	defaultPath := "terraform/main.tf"
	if isTerraformFilePath(cfg.DefaultPath) {
		defaultPath = cfg.DefaultPath
	}
	resolvedPath = repoScopedPath(rootPath, firstNonEmpty(opp.IaCPath, defaultPath))
	return resolvedRepo, resolvedPath, cfg, nil
}

func buildIaCPlan(opp store.CostOpportunity, repo, path, actor string) store.IaCPullRequest {
	resourceName := strings.ToLower(strings.TrimSpace(opp.Name))
	if resourceName == "" {
		resourceName = strings.ToLower(opp.ResourceID)
	}
	resourceName = strings.Trim(branchSafe.ReplaceAllString(resourceName, "-"), "-")
	if resourceName == "" {
		resourceName = "resource"
	}
	branch := fmt.Sprintf("consize/%s/%s", opp.Action, resourceName)
	title := fmt.Sprintf("Remove %s %s", humanResourceType(opp.ResourceType), opp.Name)
	if strings.TrimSpace(opp.Name) == "" {
		title = fmt.Sprintf("Remove %s %s", humanResourceType(opp.ResourceType), opp.ResourceID)
	}
	if path == "" {
		path = "terraform/main.tf"
	}
	diff := terraformDiff(opp, path)
	body := fmt.Sprintf(`## Summary

Consize found a cloud resource that appears to be accruing cost without serving traffic or workload demand.

- Resource: %s
- Type: %s
- Provider: %s
- Region: %s
- Estimated savings: $%.2f/mo
- Recommended action: %s

## Safety

This PR path avoids direct cloud-console changes. Review the Terraform address and evidence before merge/apply.

## Evidence

%s
`, displayName(opp), humanResourceType(opp.ResourceType), opp.Provider, opp.Region, opp.MonthlyCost, opp.Action, evidenceMarkdown(opp.Evidence))

	return store.IaCPullRequest{
		OpportunityID: opp.ID,
		ChangeKind:    store.IaCChangeKindCostOpportunity,
		Actor:         actor,
		Provider:      "terraform",
		Repo:          repo,
		Branch:        branch,
		Title:         title,
		Body:          body,
		Diff:          diff,
		Status:        store.IaCPRPlanned,
	}
}

func buildRecommendationIaCPlan(rec store.Recommendation, wl store.Workload, repo, path, addr, actor string) store.IaCPullRequest {
	if path == "" {
		path = defaultRecommendationIaCPath(wl)
	}
	if addr == "" {
		addr = defaultRecommendationTerraformAddr(rec, wl)
	}
	branch := fmt.Sprintf("consize/rightsize/%s-%s-%s", safeSlug(wl.Namespace), safeSlug(wl.Name), safeSlug(rec.Resource))
	title := fmt.Sprintf("Rightsize %s/%s %s", wl.Namespace, wl.Name, rec.Resource)
	provider := iacProviderForPath(path)
	diff := recommendationIaCDiff(rec, wl, path, addr, provider)
	body := fmt.Sprintf(`## Summary

Consize recommends changing this through the team's source repository instead of directly patching the live runtime. This avoids configuration drift when infrastructure or workloads are managed as code.

- Workload: %s/%s
- Kind: %s
- Resource: %s
- Change: %s
- Estimated savings: $%.2f/mo
- Source type: %s

## Delivery choice

Use this PR path when the team manages this resource with Terraform, Kubernetes YAML, Helm values, Kustomize, or another GitOps source. Direct apply remains available for non-IaC workloads or break-glass convenience.

## Review notes

- Confirm the source file and resource target before opening the PR.
- Merge through the team's normal review process.
- Let the next Consize collection/analyze cycle verify that live state and desired state converge.
`, wl.Namespace, wl.Name, wl.Kind, rec.Resource, recommendationChange(rec), rec.SavingsMonthly, humanIaCProvider(provider))
	return store.IaCPullRequest{
		RecommendationID: rec.ID,
		ChangeKind:       store.IaCChangeKindRecommendation,
		Actor:            actor,
		Provider:         provider,
		Repo:             repo,
		Branch:           branch,
		Title:            title,
		Body:             body,
		Diff:             diff,
		Status:           store.IaCPRPlanned,
	}
}

func defaultRecommendationIaCPath(wl store.Workload) string {
	switch wl.Source {
	case "db":
		return "terraform/databases.tf"
	default:
		return "terraform/workloads.tf"
	}
}

func normalizeRecommendationIaCInput(repo, path, addr string) (string, string, string) {
	repo = strings.TrimSpace(repo)
	path = strings.TrimSpace(path)
	addr = strings.TrimSpace(addr)
	if looksLikeURL(path) {
		if repo == "" {
			repo = path
		}
		path = ""
	}
	return repo, strings.TrimLeft(path, "/"), addr
}

func validateIaCFilePath(path string) error {
	path = cleanRepoPath(path)
	if path == "" {
		return fmt.Errorf("source file path is required")
	}
	if !isSupportedIaCFilePath(path) {
		return fmt.Errorf("source file path must point to a supported IaC file (.tf, .tf.json, .yaml, or .yml), not a directory. Examples: infra/terraform/workloads.tf or kubernetes/apps/deployment.yaml")
	}
	return nil
}

func validateTerraformFilePath(path string) error {
	path = cleanRepoPath(path)
	if path == "" {
		return fmt.Errorf("Terraform file path is required")
	}
	if !isTerraformFilePath(path) {
		return fmt.Errorf("cloud waste cleanup currently needs a Terraform source file (.tf or .tf.json), not a directory or Kubernetes manifest")
	}
	return nil
}

func isTerraformFilePath(path string) bool {
	path = strings.ToLower(cleanRepoPath(path))
	return strings.HasSuffix(path, ".tf") || strings.HasSuffix(path, ".tf.json")
}

func isYAMLFilePath(path string) bool {
	path = strings.ToLower(cleanRepoPath(path))
	return strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")
}

func isSupportedIaCFilePath(path string) bool {
	return isTerraformFilePath(path) || isYAMLFilePath(path)
}

func iacProviderForPath(path string) string {
	path = strings.ToLower(cleanRepoPath(path))
	if strings.Contains(path, "values.") && isYAMLFilePath(path) {
		return "helm-values"
	}
	if isYAMLFilePath(path) {
		return "kubernetes-yaml"
	}
	return "terraform"
}

func humanIaCProvider(provider string) string {
	switch provider {
	case "kubernetes-yaml":
		return "Kubernetes YAML"
	case "helm-values":
		return "Helm values"
	default:
		return "Terraform"
	}
}

func kubernetesManifestKind(wl store.Workload) string {
	switch strings.ToLower(strings.TrimSpace(wl.Kind)) {
	case "statefulset", "statefulsets":
		return "StatefulSet"
	case "daemonset", "daemonsets":
		return "DaemonSet"
	case "job", "jobs":
		return "Job"
	case "cronjob", "cronjobs":
		return "CronJob"
	default:
		return "Deployment"
	}
}

func looksLikeURL(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "git@")
}

func defaultRecommendationTerraformAddr(rec store.Recommendation, wl store.Workload) string {
	name := safeTerraformName(wl.Name)
	switch rec.Resource {
	case store.ResourceClass:
		if wl.DBProvider == "gcp" {
			return "google_sql_database_instance." + name
		}
		return "aws_db_instance." + name
	default:
		return "kubernetes_deployment." + name
	}
}

func recommendationTerraformDiff(rec store.Recommendation, wl store.Workload, path, addr string) string {
	kind, name := terraformAddrParts(addr)
	switch rec.Resource {
	case store.ResourceClass:
		attr := "instance_class"
		if wl.DBProvider == "gcp" {
			attr = "settings.tier"
		}
		return fmt.Sprintf(`diff --git a/%s b/%s
--- a/%s
+++ b/%s
@@
 resource %q %q {
-%s = %q
+%s = %q
 }
`, path, path, path, path, kind, name, attr, rec.ClassCurrent, attr, rec.ClassProposed)
	case store.ResourceMemory:
		return fmt.Sprintf(`diff --git a/%s b/%s
--- a/%s
+++ b/%s
@@
 resource %q %q {
   spec {
     template {
       spec {
         container {
           resources {
-            requests = { memory = %q }
-            limits   = { memory = %q }
+            requests = { memory = %q }
+            limits   = { memory = %q }
           }
         }
       }
     }
   }
 }
`, path, path, path, path, kind, name, mib(rec.CurrentValue), mib(rec.CurrentLimit), mib(rec.ProposedValue), mib(rec.ProposedLimit))
	default:
		return fmt.Sprintf(`diff --git a/%s b/%s
--- a/%s
+++ b/%s
@@
 resource %q %q {
   spec {
     template {
       spec {
         container {
           resources {
-            requests = { cpu = %q }
-            limits   = { cpu = %q }
+            requests = { cpu = %q }
+            limits   = { cpu = %q }
           }
         }
       }
     }
   }
 }
`, path, path, path, path, kind, name, milli(rec.CurrentValue), milli(rec.CurrentLimit), milli(rec.ProposedValue), milli(rec.ProposedLimit))
	}
}

func recommendationIaCDiff(rec store.Recommendation, wl store.Workload, path, addr, provider string) string {
	switch provider {
	case "kubernetes-yaml":
		return recommendationKubernetesYAMLDiff(rec, wl, path)
	case "helm-values":
		return recommendationHelmValuesDiff(rec, wl, path)
	default:
		return recommendationTerraformDiff(rec, wl, path, addr)
	}
}

func recommendationKubernetesYAMLDiff(rec store.Recommendation, wl store.Workload, path string) string {
	kind := kubernetesManifestKind(wl)
	if rec.Resource == store.ResourceMemory {
		return fmt.Sprintf(`diff --git a/%s b/%s
--- a/%s
+++ b/%s
@@
 kind: %s
 metadata:
   name: %s
 spec:
   template:
     spec:
       containers:
       - resources:
           requests:
-            memory: %q
+            memory: %q
           limits:
-            memory: %q
+            memory: %q
`, path, path, path, path, kind, wl.Name, mib(rec.CurrentValue), mib(rec.ProposedValue), mib(rec.CurrentLimit), mib(rec.ProposedLimit))
	}
	return fmt.Sprintf(`diff --git a/%s b/%s
--- a/%s
+++ b/%s
@@
 kind: %s
 metadata:
   name: %s
 spec:
   template:
     spec:
       containers:
       - resources:
           requests:
-            cpu: %q
+            cpu: %q
           limits:
-            cpu: %q
+            cpu: %q
`, path, path, path, path, kind, wl.Name, milli(rec.CurrentValue), milli(rec.ProposedValue), milli(rec.CurrentLimit), milli(rec.ProposedLimit))
}

func recommendationHelmValuesDiff(rec store.Recommendation, wl store.Workload, path string) string {
	if rec.Resource == store.ResourceMemory {
		return fmt.Sprintf(`diff --git a/%s b/%s
--- a/%s
+++ b/%s
@@
 # Helm values target: %s/%s
 resources:
   requests:
-    memory: %q
+    memory: %q
   limits:
-    memory: %q
+    memory: %q
`, path, path, path, path, wl.Namespace, wl.Name, mib(rec.CurrentValue), mib(rec.ProposedValue), mib(rec.CurrentLimit), mib(rec.ProposedLimit))
	}
	return fmt.Sprintf(`diff --git a/%s b/%s
--- a/%s
+++ b/%s
@@
 # Helm values target: %s/%s
 resources:
   requests:
-    cpu: %q
+    cpu: %q
   limits:
-    cpu: %q
+    cpu: %q
`, path, path, path, path, wl.Namespace, wl.Name, milli(rec.CurrentValue), milli(rec.ProposedValue), milli(rec.CurrentLimit), milli(rec.ProposedLimit))
}

func recommendationChange(rec store.Recommendation) string {
	if rec.Resource == store.ResourceClass {
		return fmt.Sprintf("%s → %s", rec.ClassCurrent, rec.ClassProposed)
	}
	if rec.Resource == store.ResourceMemory {
		return fmt.Sprintf("%s → %s", mib(rec.CurrentValue), mib(rec.ProposedValue))
	}
	return fmt.Sprintf("%s → %s", milli(rec.CurrentValue), milli(rec.ProposedValue))
}

func terraformAddrParts(addr string) (string, string) {
	parts := strings.Split(strings.TrimSpace(addr), ".")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if p := strings.TrimSpace(part); p != "" {
			clean = append(clean, p)
		}
	}
	if len(clean) < 2 {
		return "kubernetes_deployment", safeTerraformName(addr)
	}
	return clean[len(clean)-2], safeTerraformName(clean[len(clean)-1])
}

func mib(v int64) string {
	if v <= 0 {
		return "0Mi"
	}
	return fmt.Sprintf("%dMi", v/(1024*1024))
}

func milli(v int64) string {
	if v <= 0 {
		return "0m"
	}
	return fmt.Sprintf("%dm", v)
}

func safeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = branchSafe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "resource"
	}
	return s
}

func safeTerraformName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = terraformNameSafe.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "resource"
	}
	return s
}

func displayName(opp store.CostOpportunity) string {
	if opp.Name != "" {
		return opp.Name + " (" + opp.ResourceID + ")"
	}
	return opp.ResourceID
}

func humanResourceType(t string) string {
	switch t {
	case costscan.TypeUnattachedVolume:
		return "unattached volume"
	case costscan.TypeIdleLoadBalancer:
		return "idle load balancer"
	case costscan.TypeUnusedNATGateway:
		return "unused NAT gateway"
	case costscan.TypeStoppedInstance:
		return "stopped instance"
	default:
		return strings.ReplaceAll(t, "_", " ")
	}
}

func evidenceMarkdown(e map[string]any) string {
	if len(e) == 0 {
		return "- No additional evidence recorded."
	}
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "- %s: %v\n", k, e[k])
	}
	return strings.TrimRight(b.String(), "\n")
}

func terraformDiff(opp store.CostOpportunity, path string) string {
	addr := opp.TerraformAddr
	if addr == "" {
		addr = fmt.Sprintf("# terraform address unknown for %s", opp.ResourceID)
	}
	return fmt.Sprintf(`diff --git a/%s b/%s
--- a/%s
+++ b/%s
@@
-resource %q %q {
-  # %s
-}
+# Removed by Consize recommendation:
+# %s
+# Estimated savings: $%.2f/mo
`, path, path, path, path, terraformKind(opp), terraformName(addr), displayName(opp), opp.Recommendation, opp.MonthlyCost)
}

func terraformKind(opp store.CostOpportunity) string {
	if kind, _, ok := strings.Cut(opp.TerraformAddr, "."); ok && strings.TrimSpace(kind) != "" {
		return kind
	}
	switch opp.ResourceType {
	case costscan.TypeUnattachedVolume:
		return "aws_ebs_volume"
	case costscan.TypeIdleLoadBalancer:
		return "aws_lb"
	case costscan.TypeUnusedNATGateway:
		return "aws_nat_gateway"
	case costscan.TypeStoppedInstance:
		return "aws_instance"
	default:
		return opp.ResourceType
	}
}

func terraformName(addr string) string {
	parts := strings.Split(addr, ".")
	if len(parts) > 1 {
		return parts[len(parts)-1]
	}
	return strings.Trim(strings.ToLower(branchSafe.ReplaceAllString(addr, "_")), "_")
}
