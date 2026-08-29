package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func put(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGitHubIntegrationConfigRoundTrip(t *testing.T) {
	t.Setenv("CONSIZE_GITHUB_TOKEN", "test-token")
	h, _ := newTestServer(t)

	rec := get(t, h, "/api/v1/integrations/github")
	if rec.Code != http.StatusOK {
		t.Fatalf("get default integration: %d %s", rec.Code, rec.Body.String())
	}
	var before struct {
		Config struct {
			Enabled  bool   `json:"enabled"`
			TokenEnv string `json:"token_env"`
		} `json:"config"`
		Source       string `json:"source"`
		TokenPresent bool   `json:"token_present"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if before.Source != "default" || before.Config.Enabled || before.Config.TokenEnv != "CONSIZE_GITHUB_TOKEN" || !before.TokenPresent {
		t.Fatalf("bad default integration body: %s", rec.Body.String())
	}

	rec = put(t, h, "/api/v1/integrations/github", map[string]any{
		"enabled":      true,
		"organization": "TheInvincibleRalph",
		"token_env":    "CONSIZE_GITHUB_TOKEN",
		"default_repo": "TheInvincibleRalph/ecommerce",
		"default_path": "/terraform/workloads.tf",
		"repositories": []map[string]any{
			{
				"alias":          "commerce",
				"repo":           "TheInvincibleRalph/ecommerce",
				"default_branch": "main",
				"root_path":      "/infra",
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put integration: %d %s", rec.Code, rec.Body.String())
	}
	var after struct {
		Config struct {
			Enabled      bool   `json:"enabled"`
			DefaultPath  string `json:"default_path"`
			Repositories []struct {
				RootPath string `json:"root_path"`
			} `json:"repositories"`
		} `json:"config"`
		Source       string `json:"source"`
		TokenPresent bool   `json:"token_present"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if !after.Config.Enabled || after.Source != "store" || !after.TokenPresent ||
		after.Config.DefaultPath != "terraform/workloads.tf" ||
		len(after.Config.Repositories) != 1 || after.Config.Repositories[0].RootPath != "infra" {
		t.Fatalf("bad saved integration body: %s", rec.Body.String())
	}
}

func TestGitHubIntegrationRejectsSecretsAndURLPaths(t *testing.T) {
	h, _ := newTestServer(t)

	rec := put(t, h, "/api/v1/integrations/github", map[string]any{
		"enabled":      true,
		"token_env":    "ghp_this_is_a_token",
		"default_repo": "platform/infra",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("raw token should be rejected: %d %s", rec.Code, rec.Body.String())
	}

	rec = put(t, h, "/api/v1/integrations/github", map[string]any{
		"enabled":      true,
		"token_env":    "CONSIZE_GITHUB_TOKEN",
		"default_repo": "platform/infra",
		"default_path": "https://github.com/platform/infra",
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "repo-relative") {
		t.Fatalf("URL path should be rejected: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRecommendationIaCPlanUsesSavedGitHubRepositoryDefaults(t *testing.T) {
	h, st := newTestServer(t)
	recID := seedPendingRec(t, st, "apps", nil)

	rec := put(t, h, "/api/v1/integrations/github", map[string]any{
		"enabled":      true,
		"token_env":    "CONSIZE_GITHUB_TOKEN",
		"default_repo": "commerce",
		"default_path": "terraform/workloads.tf",
		"repositories": []map[string]any{
			{
				"alias":     "commerce",
				"repo":      "TheInvincibleRalph/ecommerce",
				"root_path": "infra/live",
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save integration: %d %s", rec.Code, rec.Body.String())
	}

	rec = post(t, h, "/api/v1/recommendations/"+itoa(recID)+"/iac-pr", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("prepare recommendation pr: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		PullRequest struct {
			Repo string `json:"Repo"`
			Diff string `json:"Diff"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.PullRequest.Repo != "TheInvincibleRalph/ecommerce" ||
		!strings.Contains(body.PullRequest.Diff, "infra/live/terraform/workloads.tf") ||
		!strings.Contains(body.PullRequest.Diff, `resource "kubernetes_deployment" "api"`) {
		t.Fatalf("saved repository default was not used: %s", rec.Body.String())
	}
}

func TestRecommendationIaCPlanAppliesRepoRootToDefaultPath(t *testing.T) {
	h, st := newTestServer(t)
	recID := seedPendingRec(t, st, "apps", nil)

	rec := put(t, h, "/api/v1/integrations/github", map[string]any{
		"enabled":      true,
		"token_env":    "CONSIZE_GITHUB_TOKEN",
		"default_repo": "commerce",
		"repositories": []map[string]any{
			{
				"alias":     "commerce",
				"repo":      "TheInvincibleRalph/ecommerce",
				"root_path": "infra/live",
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save integration: %d %s", rec.Code, rec.Body.String())
	}

	rec = post(t, h, "/api/v1/recommendations/"+itoa(recID)+"/iac-pr", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("prepare recommendation pr: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		PullRequest struct {
			Repo string `json:"Repo"`
			Diff string `json:"Diff"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.PullRequest.Diff, "infra/live/terraform/workloads.tf") {
		t.Fatalf("repo root was not applied to default Terraform path: %s", rec.Body.String())
	}
}

func TestRecommendationIaCPlanAvoidsDuplicateTerraformPathSegment(t *testing.T) {
	h, st := newTestServer(t)
	recID := seedPendingRec(t, st, "apps", nil)

	rec := put(t, h, "/api/v1/integrations/github", map[string]any{
		"organization": "TheInvincibleRalph",
		"enabled":      true,
		"token_env":    "CONSIZE_GITHUB_TOKEN",
		"default_repo": "infra",
		"repositories": []map[string]any{
			{
				"alias":     "infra",
				"repo":      "Enterprise-grade-GKE-Project",
				"root_path": "infra/terraform",
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save integration: %d %s", rec.Code, rec.Body.String())
	}

	rec = post(t, h, "/api/v1/recommendations/"+itoa(recID)+"/iac-pr", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("prepare recommendation pr: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		PullRequest struct {
			Repo string `json:"Repo"`
			Diff string `json:"Diff"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.PullRequest.Repo != "TheInvincibleRalph/Enterprise-grade-GKE-Project" ||
		strings.Contains(body.PullRequest.Diff, "infra/terraform/terraform/") ||
		!strings.Contains(body.PullRequest.Diff, "infra/terraform/workloads.tf") {
		t.Fatalf("repo root duplicated Terraform path segment: %s", rec.Body.String())
	}
}
