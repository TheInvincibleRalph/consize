package verifier_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"consize/internal/collector"
	"consize/internal/store"
	"consize/internal/verifier"
)

// fakeProm serves canned query_range results per window: the verifier
// queries the same PromQL twice (baseline window ending at apply time,
// post window starting at apply time), so the fake picks the data set
// from whether the requested range starts before the apply time.
type fakeProm struct {
	mu        sync.Mutex
	applyTime time.Time
	baseline  map[string][]collector.Series
	post      map[string][]collector.Series
}

func (f *fakeProm) QueryRange(_ context.Context, query string, start, _ time.Time, _ time.Duration) ([]collector.Series, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if start.Before(f.applyTime) {
		return f.baseline[query], nil
	}
	return f.post[query], nil
}

func (f *fakeProm) setBaseline(query string, values ...float64) { f.set(&f.baseline, query, values...) }
func (f *fakeProm) setPost(query string, values ...float64)     { f.set(&f.post, query, values...) }

func (f *fakeProm) set(dst *map[string][]collector.Series, query string, values ...float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if *dst == nil {
		*dst = map[string][]collector.Series{}
	}
	(*dst)[query] = []collector.Series{{Points: points(values...)}}
}

func points(values ...float64) []collector.Point {
	out := make([]collector.Point, len(values))
	for i, v := range values {
		out[i] = collector.Point{Timestamp: time.Now(), Value: v}
	}
	return out
}

// run one verification and return the verdict plus rollback calls.
func verify(t *testing.T, st store.Store, prom *fakeProm, rollbacks *[]int64, event store.ApplyEvent) verifier.Verdict {
	t.Helper()
	prom.applyTime = event.CreatedAt // the fake splits windows at the apply time
	cfg := verifier.DefaultConfig()
	cfg.SustainedMinutes = 5
	s := verifier.New(prom, st, func(_ context.Context, e store.ApplyEvent) error {
		*rollbacks = append(*rollbacks, e.ID)
		return nil
	}, cfg)
	v, err := s.Verify(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func seedAppliedEvent(t *testing.T, st store.Store) (store.ApplyEvent, string) {
	t.Helper()
	ctx := context.Background()
	wlID, err := st.UpsertWorkload(ctx, store.Workload{Name: "api", Namespace: "prod", Kind: "deployment", Source: "k8s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRecommendations(ctx, []store.Recommendation{{
		WorkloadID: wlID, Resource: "cpu",
		CurrentValue: 4000, ProposedValue: 2800,
		CurrentLimit: 8000, ProposedLimit: 6400,
		SavingsMonthly: 10, Confidence: 0.9, PolicyVersion: "v1",
	}}); err != nil {
		t.Fatal(err)
	}
	recs, _, err := st.ListRecommendations(ctx, &wlID, store.StatusPending, 0, 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("seed rec: %v %d", err, len(recs))
	}
	eid, err := st.CreateApplyEvent(ctx, store.ApplyEvent{
		RecommendationID: recs[0].ID, WorkloadID: wlID,
		Actor: "alice", Mode: "approved", Result: store.EventApplied,
		Diff:      store.Diff{Resource: "cpu", CurrentReq: 4000, ProposedReq: 2800, CurrentLimit: 8000, ProposedLimit: 6400},
		CreatedAt: time.Now().Add(-30 * time.Hour).UTC(), // window long since due
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := st.GetApplyEvent(ctx, eid)
	if err != nil {
		t.Fatal(err)
	}
	return e, "prod"
}

func TestVerdictPassed(t *testing.T) {
	st := store.NewMemory()
	prom := &fakeProm{}
	event, ns := seedAppliedEvent(t, st)

	throttleQ := throttleQuery(ns)
	// Baseline 5 samples, post identical: no sustained elevation.
	prom.setBaseline(throttleQ, 0.1, 0.1, 0.1, 0.1, 0.1)
	prom.setPost(throttleQ, 0.1, 0.1, 0.1, 0.1, 0.1)
	// Counters: no OOMs, restarts, or evictions in either window.
	for _, q := range []string{oomQuery(ns), restartQuery(ns), evictQuery(ns)} {
		prom.setBaseline(q, 0, 0, 0, 0, 0)
		prom.setPost(q, 0, 0, 0, 0, 0)
	}

	var rollbacks []int64
	v := verify(t, st, prom, &rollbacks, event)
	if v.String() != store.VerdictPassed {
		t.Fatalf("verdict: %s (slis %v)", v.String(), v.SLIs)
	}
	if len(rollbacks) != 0 {
		t.Fatalf("passed verdict must not roll back: %v", rollbacks)
	}
	runs, err := st.ListVerificationRuns(context.Background(), &event.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("verification run recorded: %d, %v", len(runs), err)
	}
	if runs[0].Verdict != store.VerdictPassed {
		t.Fatalf("stored verdict: %q", runs[0].Verdict)
	}
}

func TestVerdictFailedSustainedThrottle(t *testing.T) {
	// 7 consecutive elevated throttling samples (>= 5 min at 1-min
	// resolution) after a quiet baseline = sustained breach = FAIL.
	st := store.NewMemory()
	prom := &fakeProm{}
	event, ns := seedAppliedEvent(t, st)

	throttleQ := throttleQuery(ns)
	prom.setBaseline(throttleQ, 0.1, 0.1, 0.1, 0.1, 0.1)
	prom.setPost(throttleQ, 0.1, 0.1, 0.1, 2.0, 2.0, 2.0, 2.0, 2.0, 2.0, 0.1)
	for _, q := range []string{oomQuery(ns), restartQuery(ns), evictQuery(ns)} {
		prom.setBaseline(q, 0, 0, 0, 0, 0)
		prom.setPost(q, 0, 0, 0, 0, 0)
	}

	var rollbacks []int64
	v := verify(t, st, prom, &rollbacks, event)
	if v.String() != store.VerdictFailed {
		t.Fatalf("verdict: %s (slis %v)", v.String(), v.SLIs)
	}
	if len(rollbacks) != 1 || rollbacks[0] != event.ID {
		t.Fatalf("auto-rollback on FAIL: %v", rollbacks)
	}
	// Threshold 0.1 × 1.0 mult = 0.1; the 6 elevated samples form one
	// 6-minute run.
	ev := v.SLIs["throttling"].(map[string]any)
	if ev["longest_breach_minutes"].(int) != 6 {
		t.Fatalf("breach run: %v", ev["longest_breach_minutes"])
	}
}

func TestVerdictFailedNewOOMs(t *testing.T) {
	// Any new OOMKilled event in the post window fails verification —
	// that is the downsize regression signal par excellence.
	st := store.NewMemory()
	prom := &fakeProm{}
	event, ns := seedAppliedEvent(t, st)

	throttleQ := throttleQuery(ns)
	prom.setBaseline(throttleQ, 0.1, 0.1, 0.1, 0.1, 0.1)
	prom.setPost(throttleQ, 0.1, 0.1, 0.1, 0.1, 0.1)
	prom.setBaseline(oomQuery(ns), 0, 0, 0, 0, 0)
	prom.setPost(oomQuery(ns), 0, 0, 1, 0, 0) // one new OOM
	for _, q := range []string{restartQuery(ns), evictQuery(ns)} {
		prom.setBaseline(q, 0, 0, 0, 0, 0)
		prom.setPost(q, 0, 0, 0, 0, 0)
	}

	var rollbacks []int64
	v := verify(t, st, prom, &rollbacks, event)
	if v.String() != store.VerdictFailed {
		t.Fatalf("verdict: %s (slis %v)", v.String(), v.SLIs)
	}
	if len(rollbacks) != 1 {
		t.Fatalf("auto-rollback on FAIL: %v", rollbacks)
	}
}

func TestVerdictPassedWhenThrottleIsBrief(t *testing.T) {
	// A 4-minute spike (4 consecutive samples) is below the 5-minute
	// sustained threshold — noisy bursts must not roll back.
	st := store.NewMemory()
	prom := &fakeProm{}
	event, ns := seedAppliedEvent(t, st)

	throttleQ := throttleQuery(ns)
	prom.setBaseline(throttleQ, 0.1, 0.1, 0.1, 0.1, 0.1)
	prom.setPost(throttleQ, 0.1, 0.1, 2.0, 2.0, 2.0, 2.0, 0.1, 0.1, 0.1)
	for _, q := range []string{oomQuery(ns), restartQuery(ns), evictQuery(ns)} {
		prom.setBaseline(q, 0, 0, 0, 0, 0)
		prom.setPost(q, 0, 0, 0, 0, 0)
	}

	var rollbacks []int64
	v := verify(t, st, prom, &rollbacks, event)
	if v.String() != store.VerdictPassed {
		t.Fatalf("brief burst must not fail: %s (slis %v)", v.String(), v.SLIs)
	}
	if len(rollbacks) != 0 {
		t.Fatal("no rollback for a passed verdict")
	}
}

func TestVerdictInconclusiveOnMissingData(t *testing.T) {
	// Data present in one window only = cannot compare = inconclusive,
	// and inconclusive never rolls back.
	st := store.NewMemory()
	prom := &fakeProm{}
	event, ns := seedAppliedEvent(t, st)

	throttleQ := throttleQuery(ns)
	prom.setBaseline(throttleQ, 0.1, 0.1, 0.1, 0.1, 0.1) // baseline only
	for _, q := range []string{oomQuery(ns), restartQuery(ns), evictQuery(ns)} {
		prom.setBaseline(q, 0, 0, 0, 0, 0)
	}

	var rollbacks []int64
	v := verify(t, st, prom, &rollbacks, event)
	if v.String() != store.VerdictInconclusive {
		t.Fatalf("verdict: %s (slis %v)", v.String(), v.SLIs)
	}
	if len(rollbacks) != 0 {
		t.Fatal("inconclusive must never roll back (ADR-006)")
	}
	runs, _ := st.ListVerificationRuns(context.Background(), &event.ID)
	if len(runs) != 1 || runs[0].Verdict != store.VerdictInconclusive {
		t.Fatalf("inconclusive must be recorded, never silent: %+v", runs)
	}
}

func TestVerdictInconclusiveWhenNothingMeasurable(t *testing.T) {
	// A cluster that emits none of the SLI metrics cannot prove
	// anything — explicit inconclusive, never a pass (never silence).
	st := store.NewMemory()
	prom := &fakeProm{}
	event, _ := seedAppliedEvent(t, st)

	// No data at all for any signal.
	var rollbacks []int64
	v := verify(t, st, prom, &rollbacks, event)
	if v.String() != store.VerdictInconclusive {
		t.Fatalf("verdict: %s (slis %v)", v.String(), v.SLIs)
	}
}

func TestVerificationRunCarriesEvidence(t *testing.T) {
	st := store.NewMemory()
	prom := &fakeProm{}
	event, ns := seedAppliedEvent(t, st)

	throttleQ := throttleQuery(ns)
	prom.setBaseline(throttleQ, 0.1, 0.1, 0.1, 0.1, 0.1)
	prom.setPost(throttleQ, 0.1, 0.1, 0.1, 0.1, 0.1)
	for _, q := range []string{oomQuery(ns), restartQuery(ns), evictQuery(ns)} {
		prom.setBaseline(q, 0, 0, 0, 0, 0)
		prom.setPost(q, 0, 0, 0, 0, 0)
	}
	var rollbacks []int64
	v := verify(t, st, prom, &rollbacks, event)

	runs, _ := st.ListVerificationRuns(context.Background(), &event.ID)
	run := runs[0]
	if run.BaselineEnd.After(event.CreatedAt) || run.PostStart.After(run.PostEnd) {
		t.Fatalf("windows: baseline %s→%s post %s→%s", run.BaselineStart, run.BaselineEnd, run.PostStart, run.PostEnd)
	}
	// Memory keeps the value as int, Postgres JSONB as float64 — compare
	// numerically either way.
	sustained, _ := run.Thresholds["sustained_minutes"].(float64)
	if len(run.SLIs) != 4 || int(sustained) != 5 && fmt.Sprint(run.Thresholds["sustained_minutes"]) != "5" {
		t.Fatalf("evidence: slis %v thresholds %v", run.SLIs, run.Thresholds)
	}
	if v.String() != run.Verdict {
		t.Fatal("returned and stored verdicts must agree")
	}
}

// --- query helpers (must mirror verifier.go's PromQL construction) ---

func throttleQuery(ns string) string {
	return `sum by (namespace) (rate(container_cpu_cfs_throttled_seconds_total{namespace="` + ns + `"}[5m]))`
}
func oomQuery(ns string) string {
	return `sum by (namespace) (increase(container_oom_events_total{namespace="` + ns + `"}[5m]))`
}
func restartQuery(ns string) string {
	return `sum by (namespace) (increase(kube_pod_container_status_restarts_total{namespace="` + ns + `"}[5m]))`
}
func evictQuery(ns string) string {
	return `sum by (namespace) (increase(kube_pod_status_reason{namespace="` + ns + `",reason="Evicted"}[5m]))`
}

// Note: baseline and post windows reuse the same PromQL string; the
// fake distinguishes them by requested time range (applyTime split).
