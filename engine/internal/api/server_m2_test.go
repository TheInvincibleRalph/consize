package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"consize/internal/api"
	"consize/internal/apply"
	"consize/internal/pricing"
	"consize/internal/store"
)

// recordingPatcher records patches like the real k8s patcher, but on
// memory — the apply endpoints are exercised end to end through the
// real guardrail pipeline.
type recordingPatcher struct {
	patches int
}

func (r *recordingPatcher) PatchDeployment(_ context.Context, _, _ string, _ store.Diff) error {
	r.patches++
	return nil
}
func (r *recordingPatcher) ReadResources(context.Context, string, string, string) (int64, int64, error) {
	return 0, 0, nil
}
func (r *recordingPatcher) Health(context.Context) error { return nil }

// newApplyServer mounts the API with a live apply engine over the
// in-memory store and a recording patcher.
func newApplyServer(t *testing.T) (http.Handler, *store.Memory, *recordingPatcher) {
	t.Helper()
	st := store.NewMemory()
	p := &recordingPatcher{}
	applier := apply.NewService(st, p, apply.DefaultConfig())
	return apiNew(st, applier), st, p
}

// apiNew mirrors newTestServer but with an applier (kept local so the
// M1 helper stays untouched).
func apiNew(st store.Store, applier *apply.Service) http.Handler {
	return api.New(st, pricing.Static{P: pricing.DefaultStatic()}, applier, nil)
}

func post(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func seedPendingRec(t *testing.T, st *store.Memory, ns string, labels map[string]string) int64 {
	t.Helper()
	ctx := context.Background()
	wlID, err := st.UpsertWorkload(ctx, store.Workload{
		Name: "api", Namespace: ns, Kind: "deployment", Source: "k8s", Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRecommendations(ctx, []store.Recommendation{{
		WorkloadID: wlID, Resource: "cpu",
		CurrentValue: 4000, ProposedValue: 1200,
		CurrentLimit: 8000, ProposedLimit: 4000,
		SavingsMonthly: 25, Confidence: 0.9, PolicyVersion: "v1",
	}}); err != nil {
		t.Fatal(err)
	}
	recs, _, err := st.ListRecommendations(ctx, &wlID, store.StatusPending, 0, 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("seed: %v %d", err, len(recs))
	}
	return recs[0].ID
}

func TestApplyEndpointDryRun(t *testing.T) {
	h, st, p := newApplyServer(t)
	recID := seedPendingRec(t, st, "prod", nil)

	rec := post(t, h, "/api/v1/recommendations/"+itoa(recID)+"/apply", map[string]string{"mode": "dry_run"})
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body.String())
	}
	var res apply.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || res.Applied || p.patches != 0 {
		t.Fatalf("dry run must not patch: %+v", res)
	}
	// A planned event landed in the trail and is visible via the API.
	events := get(t, h, "/api/v1/applies")
	if !bytes.Contains(events.Body.Bytes(), []byte(`"Result":"planned"`)) {
		t.Fatalf("trail missing planned event: %s", events.Body.String())
	}
}

func TestApplyEndpointApproved(t *testing.T) {
	h, st, p := newApplyServer(t)
	recID := seedPendingRec(t, st, "prod", nil)

	rec := post(t, h, "/api/v1/recommendations/"+itoa(recID)+"/apply",
		map[string]string{"mode": "approved", "actor": "alice"})
	if rec.Code != http.StatusOK {
		t.Fatalf("approved apply: %d %s", rec.Code, rec.Body.String())
	}
	if p.patches != 1 {
		t.Fatalf("expected one patch, got %d", p.patches)
	}
}

func TestApplyEndpointBlockedReturnsReasons(t *testing.T) {
	h, st, _ := newApplyServer(t)
	recID := seedPendingRec(t, st, "prod", map[string]string{"consize.savings.dev/exclude": "true"})

	rec := post(t, h, "/api/v1/recommendations/"+itoa(recID)+"/apply",
		map[string]string{"mode": "approved", "actor": "alice"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blocked apply: %d %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("excluded by label")) {
		t.Fatalf("reasons missing: %s", rec.Body.String())
	}
}

func TestApplyEndpointWithoutApplier(t *testing.T) {
	h, _ := newTestServer(t) // M1 server, no applier
	rec := post(t, h, "/api/v1/recommendations/1/apply", map[string]string{"mode": "approved", "actor": "alice"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without a write identity, got %d", rec.Code)
	}
}

func TestAppliesAndVerificationEndpoints(t *testing.T) {
	h, st, _ := newApplyServer(t)
	recID := seedPendingRec(t, st, "prod", nil)
	if rec := post(t, h, "/api/v1/recommendations/"+itoa(recID)+"/apply",
		map[string]string{"mode": "approved", "actor": "alice"}); rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}

	// The applied event shows in /applies with its diff.
	applies := get(t, h, "/api/v1/applies")
	if !bytes.Contains(applies.Body.Bytes(), []byte(`"Result":"applied"`)) {
		t.Fatalf("applies missing applied event: %s", applies.Body.String())
	}

	// Verification runs list (empty so far) — the endpoint must exist.
	runs := get(t, h, "/api/v1/verification-runs")
	if runs.Code != http.StatusOK || !bytes.Contains(runs.Body.Bytes(), []byte(`"verification_runs":[]`)) {
		t.Fatalf("verification-runs: %d %s", runs.Code, runs.Body.String())
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }
