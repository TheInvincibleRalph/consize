package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCostOpportunityScanAndList(t *testing.T) {
	t.Setenv("CONSIZE_COSTSCAN", "fixture")
	h, _ := newTestServer(t)

	rec := post(t, h, "/api/v1/cost-opportunities/scan", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("scan: %d %s", rec.Code, rec.Body.String())
	}
	var scan struct {
		Opportunities []map[string]any `json:"opportunities"`
		Source        string           `json:"source"`
		Summary       struct {
			Count          int     `json:"count"`
			MonthlySavings float64 `json:"monthly_savings"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &scan); err != nil {
		t.Fatal(err)
	}
	if scan.Source != "fixture" || scan.Summary.Count != 4 || scan.Summary.MonthlySavings <= 0 {
		t.Fatalf("bad scan body: %s", rec.Body.String())
	}

	rec = get(t, h, "/api/v1/cost-opportunities?status=open")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Opportunities []map[string]any `json:"opportunities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Opportunities) != 4 {
		t.Fatalf("want 4 opportunities, got %d", len(list.Opportunities))
	}
}

func TestPrepareIaCPullRequestPlan(t *testing.T) {
	t.Setenv("CONSIZE_COSTSCAN", "fixture")
	h, _ := newTestServer(t)
	rec := post(t, h, "/api/v1/cost-opportunities/scan", map[string]any{})
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
		t.Fatalf("prepare pr: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		PullRequest struct {
			Status string `json:"Status"`
			Repo   string `json:"Repo"`
			Branch string `json:"Branch"`
			Diff   string `json:"Diff"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.PullRequest.Status != "planned" || body.PullRequest.Repo == "" ||
		!strings.HasPrefix(body.PullRequest.Branch, "consize/") ||
		!strings.Contains(body.PullRequest.Diff, "diff --git") {
		t.Fatalf("bad pr body: %s", rec.Body.String())
	}
}

func TestPrepareRecommendationIaCPullRequestPlan(t *testing.T) {
	h, st := newTestServer(t)
	recID := seedPendingRec(t, st, "apps", nil)

	rec := post(t, h, "/api/v1/recommendations/"+itoa(recID)+"/iac-pr", map[string]any{
		"repo":           "platform/infra-live",
		"path":           "apps/api.tf",
		"terraform_addr": "kubernetes_deployment.api",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("prepare recommendation pr: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		PullRequest struct {
			RecommendationID int64  `json:"RecommendationID"`
			ChangeKind       string `json:"ChangeKind"`
			Status           string `json:"Status"`
			Repo             string `json:"Repo"`
			Branch           string `json:"Branch"`
			Diff             string `json:"Diff"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.PullRequest.RecommendationID != recID ||
		body.PullRequest.ChangeKind != "recommendation" ||
		body.PullRequest.Status != "planned" ||
		body.PullRequest.Repo != "platform/infra-live" ||
		!strings.HasPrefix(body.PullRequest.Branch, "consize/rightsize/") ||
		!strings.Contains(body.PullRequest.Diff, "kubernetes_deployment") ||
		!strings.Contains(body.PullRequest.Diff, "1200m") {
		t.Fatalf("bad recommendation pr body: %s", rec.Body.String())
	}
}

func TestPrepareRecommendationIaCPullRequestTreatsURLPathAsRepo(t *testing.T) {
	h, st := newTestServer(t)
	recID := seedPendingRec(t, st, "apps", nil)

	rec := post(t, h, "/api/v1/recommendations/"+itoa(recID)+"/iac-pr", map[string]any{
		"path": "https://github.com/TheInvincibleRalph/ecommerce",
	})
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
	if body.PullRequest.Repo != "https://github.com/TheInvincibleRalph/ecommerce" {
		t.Fatalf("URL path should be treated as repo, got %q", body.PullRequest.Repo)
	}
	if strings.Contains(body.PullRequest.Diff, "github.com/TheInvincibleRalph/ecommerce") ||
		!strings.Contains(body.PullRequest.Diff, "terraform/workloads.tf") {
		t.Fatalf("URL must not be used as a Terraform file path: %s", body.PullRequest.Diff)
	}
}

func TestPrepareRecommendationIaCPullRequestRejectsDirectoryPath(t *testing.T) {
	h, st := newTestServer(t)
	recID := seedPendingRec(t, st, "apps", nil)

	rec := post(t, h, "/api/v1/recommendations/"+itoa(recID)+"/iac-pr", map[string]any{
		"path": "infra/terraform",
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "supported IaC file") {
		t.Fatalf("directory path should be rejected: %d %s", rec.Code, rec.Body.String())
	}
}
