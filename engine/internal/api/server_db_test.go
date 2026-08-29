package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"consize/internal/api"
	"consize/internal/dbapply"
	"consize/internal/pricing"
	"consize/internal/store"
)

// seedDBRec writes a database workload plus a pending xlarge→micro
// class recommendation. With withWindow=true the workload gets a
// maintenance window covering the moment the test runs (start = today
// 00:00 UTC, end = tomorrow 00:00 UTC — deterministic without a clock
// hook). Returns the recommendation ID.
func seedDBRec(t *testing.T, st *store.Memory, withWindow bool) int64 {
	t.Helper()
	ctx := context.Background()
	window := ""
	if withWindow {
		now := time.Now().UTC()
		start := strings.ToLower(now.Weekday().String())[:3] + ":00:00"
		end := strings.ToLower(now.Add(24 * time.Hour).Weekday().String())[:3] + ":00:00"
		window = start + "-" + end
	}
	wlID, err := st.UpsertWorkload(ctx, store.Workload{
		Name: "payments-prod", Namespace: "db", Kind: "database", Source: "db",
		DBClass: "db.t3.xlarge", DBMaintenanceWindow: window,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRecommendations(ctx, []store.Recommendation{{
		WorkloadID: wlID, Resource: store.ResourceClass,
		ClassCurrent: "db.t3.xlarge", ClassProposed: "db.t3.micro",
		SavingsMonthly: 180, Confidence: 0.9, PolicyVersion: "v1",
	}}); err != nil {
		t.Fatal(err)
	}
	recs, _, err := st.ListRecommendations(ctx, &wlID, store.StatusPending, 0, 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("seed: %v %d", err, len(recs))
	}
	return recs[0].ID
}

// TestApplyClassRoutesToDBEngine: a class recommendation goes through
// the DB engine end to end — the one-class-step plan is visible in the
// response, a planned event is recorded, and nothing is touched.
func TestApplyClassRoutesToDBEngine(t *testing.T) {
	st := store.NewMemory()
	recID := seedDBRec(t, st, true)
	handler := api.New(st, pricing.Static{P: pricing.DefaultStatic()}, nil,
		dbapply.NewService(st, dbapply.StubChanger{}, dbapply.DefaultConfig()))

	rec := post(t, handler, "/api/v1/recommendations/"+itoa(recID)+"/apply",
		map[string]string{"mode": "dry_run"})
	if rec.Code != http.StatusOK {
		t.Fatalf("dry-run class apply: want 200, got %d %s", rec.Code, rec.Body)
	}
	var res struct {
		DryRun     bool   `json:"DryRun"`
		Applied    bool   `json:"Applied"`
		StepNumber int    `json:"StepNumber"`
		TotalSteps int    `json:"TotalSteps"`
		FollowUpID int64  `json:"FollowUpID"`
		InWindow   bool   `json:"InWindow"`
		Window     string `json:"Window"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || res.Applied {
		t.Fatalf("dry-run must plan, not apply: %+v", res)
	}
	// xlarge → micro is 4 adjacent steps; this apply plans the first
	// (xlarge → large). No follow-up on a dry-run: it must not mutate
	// the recommendation set (a real apply queues it).
	if res.StepNumber != 1 || res.TotalSteps != 4 || res.FollowUpID != 0 {
		t.Fatalf("one-class-step plan: %+v", res)
	}
	if !res.InWindow || res.Window == "" {
		t.Fatalf("maintenance window state must be reported: %+v", res)
	}

	// The planned event records the adjacent step; the workload's class
	// is untouched by a dry-run.
	events, err := st.ListApplyEvents(context.Background(), nil, "")
	if err != nil || len(events) != 1 {
		t.Fatalf("want 1 planned event, got %d err=%v", len(events), err)
	}
	if events[0].Result != store.EventPlanned || events[0].Mode != "dry_run" {
		t.Fatalf("planned event: %+v", events[0])
	}
	if events[0].Diff.Resource != store.ResourceClass ||
		events[0].Diff.ClassCurrent != "db.t3.xlarge" ||
		events[0].Diff.ClassProposed != "db.t3.large" {
		t.Fatalf("class diff on planned event: %+v", events[0].Diff)
	}
	if wl, err := st.GetWorkload(context.Background(), events[0].WorkloadID); err != nil || wl.DBClass != "db.t3.xlarge" {
		t.Fatalf("dry-run must not change the class: %+v err=%v", wl, err)
	}
}

// TestApplyClassWithoutDBEngineReturns503: no DB write identity → the
// DB apply surface answers 503, like the k8s side.
func TestApplyClassWithoutDBEngineReturns503(t *testing.T) {
	st := store.NewMemory()
	recID := seedDBRec(t, st, true)
	handler := api.New(st, pricing.Static{P: pricing.DefaultStatic()}, nil, nil)

	rec := post(t, handler, "/api/v1/recommendations/"+itoa(recID)+"/apply",
		map[string]string{"mode": "approved", "actor": "alice"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d %s", rec.Code, rec.Body)
	}
}

// TestApplyClassBlockedWithoutWindow: an unconfigured maintenance
// window is fail-closed for every mode, dry-run included — the
// guardrail reasons come back as structured 422.
func TestApplyClassBlockedWithoutWindow(t *testing.T) {
	st := store.NewMemory()
	recID := seedDBRec(t, st, false) // no maintenance window configured
	handler := api.New(st, pricing.Static{P: pricing.DefaultStatic()}, nil,
		dbapply.NewService(st, dbapply.StubChanger{}, dbapply.DefaultConfig()))

	rec := post(t, handler, "/api/v1/recommendations/"+itoa(recID)+"/apply",
		map[string]string{"mode": "dry_run"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 (fail-closed), got %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Error   string   `json:"error"`
		Reasons []string `json:"reasons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error != "apply blocked" || len(body.Reasons) == 0 {
		t.Fatalf("structured guardrail reasons: %+v", body)
	}
}

// TestApplyK8sRecDoesNotReachDBEngine: a cpu recommendation never
// consults the DB engine — with only the DB engine configured the k8s
// surface still answers 503 (asymmetric routing, one write surface per
// kind).
func TestApplyK8sRecDoesNotReachDBEngine(t *testing.T) {
	st := store.NewMemory()
	recID := seedPendingRec(t, st, "prod", nil) // cpu recommendation
	handler := api.New(st, pricing.Static{P: pricing.DefaultStatic()}, nil,
		dbapply.NewService(st, dbapply.StubChanger{}, dbapply.DefaultConfig()))

	rec := post(t, handler, "/api/v1/recommendations/"+itoa(recID)+"/apply",
		map[string]string{"mode": "dry_run"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("cpu rec must not route to the DB engine: want 503, got %d %s", rec.Code, rec.Body)
	}
}

// TestListRecommendationsSurfacesClassFields: the dashboard payload for
// a class recommendation carries the class pair (v1 PascalCase wire
// format, snake_case deferred to M4).
func TestListRecommendationsSurfacesClassFields(t *testing.T) {
	st := store.NewMemory()
	seedDBRec(t, st, true)
	handler := api.New(st, pricing.Static{P: pricing.DefaultStatic()}, nil, nil)

	rec := get(t, handler, "/api/v1/recommendations")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var payload struct {
		Recommendations []map[string]any `json:"recommendations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Recommendations) != 1 {
		t.Fatalf("want 1 recommendation, got %d", len(payload.Recommendations))
	}
	r := payload.Recommendations[0]
	if r["Resource"] != store.ResourceClass {
		t.Fatalf("resource: %v", r["Resource"])
	}
	if r["ClassCurrent"] != "db.t3.xlarge" || r["ClassProposed"] != "db.t3.micro" {
		t.Fatalf("class pair: %v / %v", r["ClassCurrent"], r["ClassProposed"])
	}
	if r["SavingsMonthly"] == nil || r["Confidence"] == nil {
		t.Fatalf("savings/confidence must ride the payload: %v", r)
	}
}

// TestReadyzCoversDBEngine: readiness consults the DB engine's health
// (the stub provider is healthy, so the endpoint stays green).
func TestReadyzCoversDBEngine(t *testing.T) {
	st := store.NewMemory()
	handler := api.New(st, pricing.Static{P: pricing.DefaultStatic()}, nil,
		dbapply.NewService(st, dbapply.StubChanger{}, dbapply.DefaultConfig()))

	rec := get(t, handler, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body)
	}
}
