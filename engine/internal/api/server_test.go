package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"consize/internal/api"
	"consize/internal/pricing"
	"consize/internal/store"
)

func newTestServer(t *testing.T) (http.Handler, *store.Memory) {
	t.Helper()
	st := store.NewMemory()
	return api.New(st, pricing.Static{P: pricing.DefaultStatic()}, nil, nil), st
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthAndReadiness(t *testing.T) {
	h, _ := newTestServer(t)
	if rec := get(t, h, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
	if rec := get(t, h, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("readyz: %d", rec.Code)
	}
}

func TestSystemStatusTelemetryFreshness(t *testing.T) {
	t.Setenv("CONSIZE_DATA_STALE_AFTER", "30m")
	ctx := context.Background()
	h, st := newTestServer(t)

	var empty struct {
		Status   string   `json:"status"`
		Messages []string `json:"messages"`
	}
	rec := get(t, h, "/api/v1/system/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty status: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Status != "empty" {
		t.Fatalf("empty status body: %s", rec.Body.String())
	}

	wid, err := st.UpsertWorkload(ctx, store.Workload{Name: "api", Namespace: "prod", Source: "k8s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBucket(ctx, store.Bucket{
		WorkloadID: wid, Metric: store.MetricCPUMilli, WindowStart: time.Now().UTC().Add(-10 * time.Minute),
		P50: 100, P95: 100, P99: 100, Max: 100, Samples: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRecommendations(ctx, []store.Recommendation{
		{WorkloadID: wid, Resource: store.ResourceCPU, CurrentValue: 1000, ProposedValue: 700, SavingsMonthly: 1, Status: store.StatusPending},
	}); err != nil {
		t.Fatal(err)
	}

	var fresh struct {
		Status                 string  `json:"status"`
		LatestUsageBucket      *string `json:"latest_usage_bucket"`
		TelemetryAgeSeconds    *int64  `json:"telemetry_age_seconds"`
		StaleAfterSeconds      int64   `json:"stale_after_seconds"`
		Workloads              int     `json:"workloads"`
		PendingRecommendations int     `json:"pending_recommendations"`
	}
	rec = get(t, h, "/api/v1/system/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh status: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.Status != "healthy" || fresh.LatestUsageBucket == nil || fresh.TelemetryAgeSeconds == nil ||
		fresh.StaleAfterSeconds != 1800 || fresh.Workloads != 1 || fresh.PendingRecommendations != 1 {
		t.Fatalf("fresh status body: %s", rec.Body.String())
	}
}

func TestSystemStatusStaleTelemetry(t *testing.T) {
	t.Setenv("CONSIZE_DATA_STALE_AFTER", "30m")
	ctx := context.Background()
	h, st := newTestServer(t)
	wid, err := st.UpsertWorkload(ctx, store.Workload{Name: "api", Namespace: "prod", Source: "k8s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBucket(ctx, store.Bucket{
		WorkloadID: wid, Metric: store.MetricCPUMilli, WindowStart: time.Now().UTC().Add(-45 * time.Minute),
		P50: 100, P95: 100, P99: 100, Max: 100, Samples: 1,
	}); err != nil {
		t.Fatal(err)
	}

	var body struct {
		Status   string   `json:"status"`
		Messages []string `json:"messages"`
	}
	rec := get(t, h, "/api/v1/system/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("stale status: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "degraded" || len(body.Messages) == 0 || !strings.Contains(body.Messages[0], "telemetry is stale") {
		t.Fatalf("stale status body: %s", rec.Body.String())
	}
}

func TestWorkloadsEndpoints(t *testing.T) {
	ctx := context.Background()
	h, st := newTestServer(t)
	id, err := st.UpsertWorkload(ctx, store.Workload{
		Name: "api", Namespace: "prod", Kind: "deployment", Source: "k8s",
		RequestCPUMilli: 1000, RequestMemBytes: 1 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := get(t, h, "/api/v1/workloads")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var list struct {
		Workloads []store.Workload `json:"workloads"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Workloads) != 1 || list.Workloads[0].Name != "api" {
		t.Fatalf("list body: %s", rec.Body.String())
	}

	rec = get(t, h, "/api/v1/workloads/"+strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}

	rec = get(t, h, "/api/v1/workloads/99999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing workload: want 404, got %d", rec.Code)
	}
	rec = get(t, h, "/api/v1/workloads/abc")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: want 400, got %d", rec.Code)
	}
}

func TestRecommendationsPagination(t *testing.T) {
	ctx := context.Background()
	h, st := newTestServer(t)
	wid, _ := st.UpsertWorkload(ctx, store.Workload{Name: "api", Namespace: "prod", Source: "k8s"})
	if err := st.CreateRecommendations(ctx, []store.Recommendation{
		{WorkloadID: wid, Resource: store.ResourceCPU, CurrentValue: 1000, ProposedValue: 300,
			SavingsMonthly: 10, Confidence: 1, Status: store.StatusPending},
		{WorkloadID: wid, Resource: store.ResourceMemory, CurrentValue: 1000, ProposedValue: 300,
			SavingsMonthly: 20, Confidence: 1, Status: store.StatusPending},
		{WorkloadID: wid, Resource: store.ResourceCPU, CurrentValue: 2000, ProposedValue: 600,
			SavingsMonthly: 30, Confidence: 1, Status: store.StatusPending},
		{WorkloadID: wid, Resource: store.ResourceMemory, CurrentValue: 2000, ProposedValue: 600,
			SavingsMonthly: 40, Confidence: 1, Status: store.StatusPending},
		{WorkloadID: wid, Resource: store.ResourceCPU, CurrentValue: 3000, ProposedValue: 900,
			SavingsMonthly: 50, Confidence: 1, Status: store.StatusPending},
	}); err != nil {
		t.Fatal(err)
	}

	type body struct {
		Recommendations []store.Recommendation `json:"recommendations"`
		Pagination      struct {
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
			Total  int `json:"total"`
		} `json:"pagination"`
	}
	decode := func(path string) body {
		t.Helper()
		rec := get(t, h, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		var b body
		if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
			t.Fatal(err)
		}
		return b
	}

	// Defaults: limit 100, offset 0, total counts before slicing.
	b := decode("/api/v1/recommendations")
	if len(b.Recommendations) != 5 || b.Pagination.Limit != 100 || b.Pagination.Offset != 0 || b.Pagination.Total != 5 {
		t.Fatalf("defaults: %s", recBody(b))
	}
	// Explicit limit+offset, ordered by savings descending.
	b = decode("/api/v1/recommendations?limit=2")
	if len(b.Recommendations) != 2 || b.Recommendations[0].SavingsMonthly != 50 ||
		b.Pagination.Limit != 2 || b.Pagination.Total != 5 {
		t.Fatalf("limit=2: %s", recBody(b))
	}
	b = decode("/api/v1/recommendations?limit=2&offset=4")
	if len(b.Recommendations) != 1 || b.Recommendations[0].SavingsMonthly != 10 ||
		b.Pagination.Offset != 4 || b.Pagination.Total != 5 {
		t.Fatalf("offset=4: %s", recBody(b))
	}
	// Oversized limits are clamped to the cap, not rejected.
	b = decode("/api/v1/recommendations?limit=9999")
	if b.Pagination.Limit != 500 {
		t.Fatalf("cap: %+v", b.Pagination)
	}

	// Invalid pagination params are 400s, never silently defaulted.
	for _, path := range []string{
		"/api/v1/recommendations?limit=0",
		"/api/v1/recommendations?limit=-1",
		"/api/v1/recommendations?limit=abc",
		"/api/v1/recommendations?offset=-1",
		"/api/v1/recommendations?offset=abc",
	} {
		if rec := get(t, h, path); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d", path, rec.Code)
		}
	}
}

// recBody formats a decoded body for failure messages.
func recBody(b any) string {
	raw, _ := json.Marshal(b)
	return string(raw)
}

func TestDashboardServedFromBinary(t *testing.T) {
	h, _ := newTestServer(t)

	// "/" serves the SPA shell.
	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("index: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("index content type: %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "Consize") {
		t.Fatalf("index body: %s", rec.Body.String()[:100])
	}

	// Assets serve with their own content types.
	for path, wantCT := range map[string]string{
		"/app.js":     "text/javascript; charset=utf-8",
		"/api.js":     "text/javascript; charset=utf-8",
		"/styles.css": "text/css; charset=utf-8",
	} {
		if rec := get(t, h, path); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, rec.Code)
		} else if ct := rec.Header().Get("Content-Type"); ct != wantCT {
			t.Fatalf("%s content type: %q want %q", path, ct, wantCT)
		}
	}

	// Unknown paths fall back to the shell (the app is hash-routed).
	if rec := get(t, h, "/some/deep/link"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "Consize") {
		t.Fatalf("SPA fallback: %d", rec.Code)
	}

	// Unknown API paths are honest 404s, never the shell.
	if rec := get(t, h, "/api/v1/does-not-exist"); rec.Code != http.StatusNotFound {
		t.Fatalf("api 404: %d", rec.Code)
	}
}

func TestRecommendationsAndSavings(t *testing.T) {
	ctx := context.Background()
	h, st := newTestServer(t)
	wid, _ := st.UpsertWorkload(ctx, store.Workload{Name: "api", Namespace: "prod", Source: "k8s"})
	if err := st.CreateRecommendations(ctx, []store.Recommendation{
		{WorkloadID: wid, Resource: store.ResourceCPU, CurrentValue: 1000, ProposedValue: 300,
			SavingsMonthly: 19.18, Confidence: 1, Status: store.StatusPending},
	}); err != nil {
		t.Fatal(err)
	}

	rec := get(t, h, "/api/v1/recommendations?status=pending")
	if rec.Code != http.StatusOK {
		t.Fatalf("recs: %d", rec.Code)
	}
	var body struct {
		Recommendations []store.Recommendation `json:"recommendations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Recommendations) != 1 || body.Recommendations[0].WorkloadName != "api" {
		t.Fatalf("recs body: %s", rec.Body.String())
	}

	rec = get(t, h, "/api/v1/savings")
	if rec.Code != http.StatusOK {
		t.Fatalf("savings: %d", rec.Code)
	}
	var sv struct {
		ProjectedMonthly float64 `json:"projected_monthly_savings"`
		Active           int     `json:"active_recommendations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sv); err != nil {
		t.Fatal(err)
	}
	if sv.ProjectedMonthly != 19.18 || sv.Active != 1 {
		t.Fatalf("savings body: %s", rec.Body.String())
	}
}
