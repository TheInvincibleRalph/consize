package api_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecommendationIaCPlanOpensGitHubPullRequestWhenTokenConfigured(t *testing.T) {
	var sawCreateRef, sawUpdateFile, sawPull bool
	var updatedContent string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/infra":
			t.Fatalf("configured base branch should avoid repository default lookup")
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/infra/git/ref/heads/release":
			writeTestJSON(w, map[string]any{"object": map[string]any{"sha": "base-sha"}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/infra/git/refs":
			sawCreateRef = true
			var body struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(body.Ref, "refs/heads/consize/rightsize/apps-api-cpu") || body.SHA != "base-sha" {
				t.Fatalf("bad create ref body: %+v", body)
			}
			w.WriteHeader(http.StatusCreated)
			writeTestJSON(w, map[string]any{"ref": body.Ref})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/infra/contents/terraform/workloads.tf":
			if r.URL.Query().Get("ref") != "release" {
				t.Fatalf("unexpected content ref %q", r.URL.Query().Get("ref"))
			}
			content := `resource "kubernetes_deployment" "api" {
  spec {
    template {
      spec {
        container {
          resources {
            requests = { cpu = "4000m" }
            limits   = { cpu = "8000m" }
          }
        }
      }
    }
  }
}
`
			writeTestJSON(w, map[string]any{
				"encoding": "base64",
				"sha":      "file-sha",
				"content":  base64.StdEncoding.EncodeToString([]byte(content)),
			})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/infra/contents/terraform/workloads.tf":
			sawUpdateFile = true
			var body struct {
				Content string `json:"content"`
				SHA     string `json:"sha"`
				Branch  string `json:"branch"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.SHA != "file-sha" || !strings.HasPrefix(body.Branch, "consize/rightsize/apps-api-cpu") {
				t.Fatalf("bad update file body: %+v", body)
			}
			decoded, err := base64.StdEncoding.DecodeString(body.Content)
			if err != nil {
				t.Fatal(err)
			}
			updatedContent = string(decoded)
			writeTestJSON(w, map[string]any{"content": map[string]any{"path": "terraform/workloads.tf"}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/infra/pulls":
			sawPull = true
			var body struct {
				Title string `json:"title"`
				Head  string `json:"head"`
				Base  string `json:"base"`
				Draft bool   `json:"draft"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(body.Title, "Rightsize apps/api cpu") || !strings.HasPrefix(body.Head, "consize/rightsize/apps-api-cpu") || body.Base != "release" || !body.Draft {
				t.Fatalf("bad pull body: %+v", body)
			}
			w.WriteHeader(http.StatusCreated)
			writeTestJSON(w, map[string]any{"html_url": "https://github.com/acme/infra/pull/42"})
		default:
			t.Fatalf("unexpected github request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer gh.Close()

	t.Setenv("CONSIZE_GITHUB_TOKEN", "test-token")
	t.Setenv("CONSIZE_GITHUB_API_BASE", gh.URL)

	h, st := newTestServer(t)
	recID := seedPendingRec(t, st, "apps", nil)
	rec := put(t, h, "/api/v1/integrations/github", map[string]any{
		"enabled":      true,
		"token_env":    "CONSIZE_GITHUB_TOKEN",
		"default_repo": "acme/infra",
		"default_path": "terraform/workloads.tf",
		"repositories": []map[string]any{
			{
				"repo":           "acme/infra",
				"default_branch": "release",
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save integration: %d %s", rec.Code, rec.Body.String())
	}

	rec = post(t, h, "/api/v1/recommendations/"+itoa(recID)+"/iac-pr", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("open pr: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		PullRequest struct {
			Status string `json:"Status"`
			URL    string `json:"URL"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.PullRequest.Status != "opened" || body.PullRequest.URL != "https://github.com/acme/infra/pull/42" {
		t.Fatalf("bad pull request response: %s", rec.Body.String())
	}
	if !sawCreateRef || !sawUpdateFile || !sawPull ||
		!strings.Contains(updatedContent, `requests = { cpu = "1200m" }`) ||
		!strings.Contains(updatedContent, `limits   = { cpu = "4000m" }`) {
		t.Fatalf("github flow incomplete; ref=%v update=%v pull=%v content=%s", sawCreateRef, sawUpdateFile, sawPull, updatedContent)
	}
}

func TestRecommendationIaCPlanDoesNotCreateGitHubBranchWhenTerraformResourceMissing(t *testing.T) {
	var sawCreateRef bool
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/infra/git/ref/heads/main":
			writeTestJSON(w, map[string]any{"object": map[string]any{"sha": "base-sha"}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/infra/contents/terraform/main.tf":
			if r.URL.Query().Get("ref") != "main" {
				t.Fatalf("unexpected content ref %q", r.URL.Query().Get("ref"))
			}
			content := `resource "google_container_cluster" "primary" {
  name = "demo"
}
`
			writeTestJSON(w, map[string]any{
				"encoding": "base64",
				"sha":      "file-sha",
				"content":  base64.StdEncoding.EncodeToString([]byte(content)),
			})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/infra/git/refs":
			sawCreateRef = true
			t.Fatalf("branch should not be created before the target Terraform resource is validated")
		default:
			t.Fatalf("unexpected github request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer gh.Close()

	t.Setenv("CONSIZE_GITHUB_TOKEN", "test-token")
	t.Setenv("CONSIZE_GITHUB_API_BASE", gh.URL)

	h, st := newTestServer(t)
	recID := seedPendingRec(t, st, "apps", nil)
	rec := put(t, h, "/api/v1/integrations/github", map[string]any{
		"enabled":      true,
		"token_env":    "CONSIZE_GITHUB_TOKEN",
		"default_repo": "acme/infra",
		"default_path": "terraform/main.tf",
		"repositories": []map[string]any{
			{
				"repo":           "acme/infra",
				"default_branch": "main",
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save integration: %d %s", rec.Code, rec.Body.String())
	}

	rec = post(t, h, "/api/v1/recommendations/"+itoa(recID)+"/iac-pr", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("prepare pr: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		PullRequest struct {
			Status string `json:"Status"`
			Error  string `json:"Error"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.PullRequest.Status != "failed" || !strings.Contains(body.PullRequest.Error, `could not find Terraform resource "kubernetes_deployment" "api"`) {
		t.Fatalf("expected target validation failure, got: %s", rec.Body.String())
	}
	if sawCreateRef {
		t.Fatal("branch was created for an invalid Terraform target")
	}
}

func TestCostOpportunityIaCPlanOpensGitHubPullRequestWhenTokenConfigured(t *testing.T) {
	var sawCreateRef, sawUpdateFile, sawPull bool
	var updatedContent string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/platform/infra-live/git/ref/heads/main":
			writeTestJSON(w, map[string]any{"object": map[string]any{"sha": "base-sha"}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/platform/infra-live/git/refs":
			sawCreateRef = true
			var body struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(body.Ref, "refs/heads/consize/remove_nat_gateway/sandbox-public-a") || body.SHA != "base-sha" {
				t.Fatalf("bad create ref body: %+v", body)
			}
			w.WriteHeader(http.StatusCreated)
			writeTestJSON(w, map[string]any{"ref": body.Ref})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/platform/infra-live/contents/aws/sandbox/network.tf":
			if r.URL.Query().Get("ref") != "main" {
				t.Fatalf("unexpected content ref %q", r.URL.Query().Get("ref"))
			}
			content := `resource "aws_nat_gateway" "sandbox_public_a" {
  allocation_id = aws_eip.sandbox.id
  subnet_id     = aws_subnet.public_a.id
}
`
			writeTestJSON(w, map[string]any{
				"encoding": "base64",
				"sha":      "file-sha",
				"content":  base64.StdEncoding.EncodeToString([]byte(content)),
			})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/platform/infra-live/contents/aws/sandbox/network.tf":
			sawUpdateFile = true
			var body struct {
				Content string `json:"content"`
				SHA     string `json:"sha"`
				Branch  string `json:"branch"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.SHA != "file-sha" || !strings.HasPrefix(body.Branch, "consize/remove_nat_gateway/sandbox-public-a") {
				t.Fatalf("bad update file body: %+v", body)
			}
			decoded, err := base64.StdEncoding.DecodeString(body.Content)
			if err != nil {
				t.Fatal(err)
			}
			updatedContent = string(decoded)
			writeTestJSON(w, map[string]any{"content": map[string]any{"path": "aws/sandbox/network.tf"}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/platform/infra-live/pulls":
			sawPull = true
			var body struct {
				Title string `json:"title"`
				Head  string `json:"head"`
				Base  string `json:"base"`
				Draft bool   `json:"draft"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(body.Title, "Remove unused NAT gateway") || !strings.HasPrefix(body.Head, "consize/remove_nat_gateway/sandbox-public-a") || body.Base != "main" || !body.Draft {
				t.Fatalf("bad pull body: %+v", body)
			}
			w.WriteHeader(http.StatusCreated)
			writeTestJSON(w, map[string]any{"html_url": "https://github.com/platform/infra-live/pull/77"})
		default:
			t.Fatalf("unexpected github request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer gh.Close()

	t.Setenv("CONSIZE_GITHUB_TOKEN", "test-token")
	t.Setenv("CONSIZE_GITHUB_API_BASE", gh.URL)

	h, _ := newTestServer(t)
	rec := put(t, h, "/api/v1/integrations/github", map[string]any{
		"enabled":   true,
		"token_env": "CONSIZE_GITHUB_TOKEN",
		"repositories": []map[string]any{
			{
				"repo":           "platform/infra-live",
				"default_branch": "main",
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save integration: %d %s", rec.Code, rec.Body.String())
	}
	rec = post(t, h, "/api/v1/cost-opportunities/scan", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("scan: %d %s", rec.Code, rec.Body.String())
	}
	var scan struct {
		Opportunities []struct {
			ID int64 `json:"ID"`
		} `json:"opportunities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &scan); err != nil {
		t.Fatal(err)
	}
	if len(scan.Opportunities) == 0 {
		t.Fatal("scan returned no opportunities")
	}

	rec = post(t, h, "/api/v1/cost-opportunities/"+itoa(scan.Opportunities[0].ID)+"/iac-pr", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("open cloud waste pr: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		PullRequest struct {
			Status string `json:"Status"`
			URL    string `json:"URL"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.PullRequest.Status != "opened" || body.PullRequest.URL != "https://github.com/platform/infra-live/pull/77" {
		t.Fatalf("bad pull request response: %s", rec.Body.String())
	}
	if !sawCreateRef || !sawUpdateFile || !sawPull ||
		strings.Contains(updatedContent, `resource "aws_nat_gateway" "sandbox_public_a"`) ||
		!strings.Contains(updatedContent, "# Removed by Consize recommendation") {
		t.Fatalf("github flow incomplete; ref=%v update=%v pull=%v content=%s", sawCreateRef, sawUpdateFile, sawPull, updatedContent)
	}
}

func writeTestJSON(w http.ResponseWriter, body any) {
	w.Header().Set("content-type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		panic(err)
	}
}
