package verifier

import (
	"context"
	"testing"
	"time"

	"consize/internal/store"
)

// seedDBMetrics writes one bucket per 15-minute step for the four DB
// metrics in both windows (baseline and post). Values are constant per
// window per metric.
func seedDBMetrics(t *testing.T, st store.Store, wlID int64, baseStart, baseEnd, postStart, postEnd time.Time,
	cpuBase, cpuPost, connsBase, connsPost, errBase, errPost float64) {
	t.Helper()
	step := DBStepMinutes * time.Minute
	seed := func(from, to time.Time, cpu, conns, errs float64) {
		for ts := from; ts.Before(to); ts = ts.Add(step) {
			for _, m := range []store.Bucket{
				{WorkloadID: wlID, Metric: store.MetricDBCPUPercent, WindowStart: ts, P95: cpu},
				{WorkloadID: wlID, Metric: store.MetricDBConnections, WindowStart: ts, P95: conns},
				{WorkloadID: wlID, Metric: store.MetricDBErrors, WindowStart: ts, P95: errs},
			} {
				if err := st.UpsertBucket(context.Background(), m); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	seed(baseStart, baseEnd, cpuBase, connsBase, errBase)
	seed(postStart, postEnd, cpuPost, connsPost, errPost)
}

// verifyDB runs one DB verification and returns the verdict plus the
// rollback calls it made. prom is nil on purpose: the DB path must not
// touch a Prometheus client at all.
func verifyDB(t *testing.T, st store.Store, rollbacks *[]int64, event store.ApplyEvent) Verdict {
	t.Helper()
	s := New(nil, st, func(_ context.Context, e store.ApplyEvent) error {
		*rollbacks = append(*rollbacks, e.ID)
		return nil
	}, DefaultConfig())
	v, err := s.VerifyDB(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// dbEvent builds an applied class event 2h ago, so the post window has
// real buckets to judge.
func dbEvent(classCurrent, classProposed string) store.ApplyEvent {
	return store.ApplyEvent{
		ID:         7,
		WorkloadID: 1,
		Actor:      "alice",
		Mode:       "approved",
		Result:     store.EventApplied,
		CreatedAt:  time.Now().UTC().Add(-2 * time.Hour),
		Diff: store.Diff{
			Resource:      store.ResourceClass,
			ClassCurrent:  classCurrent,
			ClassProposed: classProposed,
		},
	}
}

func TestVerifyDBPassHealthyDownsize(t *testing.T) {
	st := store.NewMemory()
	wlID, err := st.UpsertWorkload(context.Background(), store.Workload{
		Name: "payments-prod", Namespace: "db", Kind: "database", Source: "db",
		DBClass: "db.t3.large",
	})
	if err != nil {
		t.Fatal(err)
	}
	event := dbEvent("db.t3.large", "db.t3.medium")
	event.WorkloadID = wlID
	apply := event.CreatedAt
	seedDBMetrics(t, st, wlID, apply.Add(-24*time.Hour), apply, apply, time.Now().UTC(),
		10, 10, // cpu 10% before and after — same demand, same vCPU count
		300, 300, // 300 connections = 25% of medium's 1200 baseline
		5, 5)

	var rollbacks []int64
	v := verifyDB(t, st, &rollbacks, event)
	if v.Failed || v.Inconclusive {
		t.Fatalf("want passed, got %+v", v)
	}
	if len(rollbacks) != 0 {
		t.Fatalf("passed verdict must not roll back: %v", rollbacks)
	}
	// Evidence recorded.
	runs, err := st.ListVerificationRuns(context.Background(), nil)
	if err != nil || len(runs) != 1 {
		t.Fatalf("want 1 verification run, got %d err=%v", len(runs), err)
	}
	if runs[0].Verdict != store.VerdictPassed {
		t.Fatalf("run verdict: %s", runs[0].Verdict)
	}
	cpu := runs[0].SLIs["cpu_saturation"].(map[string]any)
	if cpu["verdict"] != "passed" || cpu["longest_breach_minutes"] != int(0) {
		t.Fatalf("cpu SLI evidence: %+v", cpu)
	}
	conns := runs[0].SLIs["connections"].(map[string]any)
	if conns["threshold"] != float64(70) {
		t.Fatalf("connections threshold should be the 70%% cap: %+v", conns)
	}
}

func TestVerifyDBFailCpuSaturation(t *testing.T) {
	st := store.NewMemory()
	wlID, _ := st.UpsertWorkload(context.Background(), store.Workload{
		Name: "bursty", Namespace: "db", Kind: "database", Source: "db",
		DBClass: "db.t3.large",
	})
	event := dbEvent("db.t3.large", "db.t3.medium")
	event.WorkloadID = wlID
	apply := event.CreatedAt
	// Post window CPU at 80% — one bucket at/above the 60% cap is a
	// 15-minute breach, well past the 5-minute sustained default. The
	// post end is a fixed 2h10m so the seeded bucket count is
	// deterministic (the verifier's own now always includes them).
	postEnd := apply.Add(2*time.Hour + 10*time.Minute)
	seedDBMetrics(t, st, wlID, apply.Add(-24*time.Hour), apply, apply, postEnd,
		10, 80,
		300, 300,
		5, 5)

	var rollbacks []int64
	v := verifyDB(t, st, &rollbacks, event)
	if !v.Failed {
		t.Fatalf("want failed, got %+v", v)
	}
	if len(rollbacks) != 1 || rollbacks[0] != event.ID {
		t.Fatalf("auto-rollback on FAIL: %v", rollbacks)
	}
	runs, _ := st.ListVerificationRuns(context.Background(), nil)
	cpu := runs[0].SLIs["cpu_saturation"].(map[string]any)
	// All 9 post-window buckets (15m steps) are at 80% → a 135-minute
	// breach.
	if cpu["longest_breach_minutes"] != int(135) {
		t.Fatalf("breach minutes: %+v", cpu)
	}
}

func TestVerifyDBFailConnectionsProjectedOnAppliedClass(t *testing.T) {
	st := store.NewMemory()
	wlID, _ := st.UpsertWorkload(context.Background(), store.Workload{
		Name: "conns", Namespace: "db", Kind: "database", Source: "db",
		DBClass: "db.t3.xlarge",
	})
	// xlarge → large: 1800 connections = 56% of xlarge's 3200 baseline,
	// but 75% of large's 2400 — the projection must use the APPLIED
	// class, or this regression would pass.
	event := dbEvent("db.t3.xlarge", "db.t3.large")
	event.WorkloadID = wlID
	apply := event.CreatedAt
	seedDBMetrics(t, st, wlID, apply.Add(-24*time.Hour), apply, apply, time.Now().UTC(),
		10, 10,
		1800, 1800, // 1800/2400 = 75% ≥ 70 → fail
		5, 5)

	var rollbacks []int64
	v := verifyDB(t, st, &rollbacks, event)
	if !v.Failed {
		t.Fatalf("want failed on connections, got %+v", v)
	}
	if len(rollbacks) != 1 {
		t.Fatalf("auto-rollback on FAIL: %v", rollbacks)
	}
	runs, _ := st.ListVerificationRuns(context.Background(), nil)
	conns := runs[0].SLIs["connections"].(map[string]any)
	if conns["verdict"] != "failed" || conns["reason"].(string) == "" {
		t.Fatalf("connections SLI evidence: %+v", conns)
	}
}

func TestVerifyDBFailErrorCounter(t *testing.T) {
	st := store.NewMemory()
	wlID, _ := st.UpsertWorkload(context.Background(), store.Workload{
		Name: "errors", Namespace: "db", Kind: "database", Source: "db",
		DBClass: "db.t3.large",
	})
	event := dbEvent("db.t3.large", "db.t3.medium")
	event.WorkloadID = wlID
	apply := event.CreatedAt
	seedDBMetrics(t, st, wlID, apply.Add(-24*time.Hour), apply, apply, time.Now().UTC(),
		10, 10,
		300, 300,
		5, 12) // errors doubled in the post window

	var rollbacks []int64
	v := verifyDB(t, st, &rollbacks, event)
	if !v.Failed {
		t.Fatalf("want failed on error counter, got %+v", v)
	}
	runs, _ := st.ListVerificationRuns(context.Background(), nil)
	errs := runs[0].SLIs["errors"].(map[string]any)
	if errs["verdict"] != "failed" {
		t.Fatalf("errors SLI evidence: %+v", errs)
	}
}

func TestVerifyDBInconclusiveMissingWindow(t *testing.T) {
	st := store.NewMemory()
	wlID, _ := st.UpsertWorkload(context.Background(), store.Workload{
		Name: "cold", Namespace: "db", Kind: "database", Source: "db",
		DBClass: "db.t3.large",
	})
	event := dbEvent("db.t3.large", "db.t3.medium")
	event.WorkloadID = wlID
	apply := event.CreatedAt
	// Only the baseline window has data — the collector hasn't ingested
	// anything since the apply.
	seedDBMetrics(t, st, wlID, apply.Add(-24*time.Hour), apply, apply.Add(-time.Hour), apply.Add(-time.Hour),
		10, 10,
		300, 300,
		5, 5)

	var rollbacks []int64
	v := verifyDB(t, st, &rollbacks, event)
	if v.Failed || !v.Inconclusive {
		t.Fatalf("want inconclusive (missing post data), got %+v", v)
	}
	if len(rollbacks) != 0 {
		t.Fatalf("no rollback for inconclusive: %v", rollbacks)
	}
}

func TestVerifyDBNoMetricsAtAllIsInconclusiveNotPass(t *testing.T) {
	st := store.NewMemory()
	wlID, _ := st.UpsertWorkload(context.Background(), store.Workload{
		Name: "unmonitored", Namespace: "db", Kind: "database", Source: "db",
		DBClass: "db.t3.large",
	})
	event := dbEvent("db.t3.large", "db.t3.medium")
	event.WorkloadID = wlID

	var rollbacks []int64
	v := verifyDB(t, st, &rollbacks, event)
	if v.Failed || !v.Inconclusive {
		t.Fatalf("want inconclusive (never silence, ADR-006), got %+v", v)
	}
}

func TestVerifyDBRejectsNonClassEvent(t *testing.T) {
	st := store.NewMemory()
	wlID, _ := st.UpsertWorkload(context.Background(), store.Workload{
		Name: "api", Namespace: "prod", Kind: "deployment", Source: "k8s",
	})
	event := dbEvent("", "")
	event.WorkloadID = wlID
	event.Diff = store.Diff{Resource: store.ResourceCPU, CurrentReq: 500, ProposedReq: 300}

	s := New(nil, st, func(_ context.Context, e store.ApplyEvent) error { return nil }, DefaultConfig())
	if _, err := s.VerifyDB(context.Background(), event); err == nil {
		t.Fatal("want error for a non-class event")
	}
}

// seedNoErrors writes cpu + connections buckets (never db_errors) across
// both windows — the shape the live CloudWatch adapter produces, which
// has no error-counter metric for RDS.
func seedNoErrors(t *testing.T, st store.Store, wlID int64, baseStart, baseEnd, postStart, postEnd time.Time,
	cpuBase, cpuPost, connsBase, connsPost float64) {
	t.Helper()
	step := DBStepMinutes * time.Minute
	seed := func(from, to time.Time, cpu, conns float64) {
		for ts := from; ts.Before(to); ts = ts.Add(step) {
			for _, m := range []store.Bucket{
				{WorkloadID: wlID, Metric: store.MetricDBCPUPercent, WindowStart: ts, P95: cpu},
				{WorkloadID: wlID, Metric: store.MetricDBConnections, WindowStart: ts, P95: conns},
			} {
				if err := st.UpsertBucket(context.Background(), m); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	seed(baseStart, baseEnd, cpuBase, connsBase)
	seed(postStart, postEnd, cpuPost, connsPost)
}

// TestVerifyDBMissingErrorsMetricIsNoEvidence: the live CloudWatch
// adapter emits no error-counter metric, so db_errors buckets are never
// ingested. A fully-missing metric must be no-evidence (SLI verdict
// "unavailable", SLI skipped) — it can never FAIL a verification that is
// otherwise healthy, and the verification still passes on the signals
// that do have data.
func TestVerifyDBMissingErrorsMetricIsNoEvidence(t *testing.T) {
	st := store.NewMemory()
	wlID, _ := st.UpsertWorkload(context.Background(), store.Workload{
		Name: "live-rds", Namespace: "db", Kind: "database", Source: "db",
		DBClass: "db.t3.large",
	})
	event := dbEvent("db.t3.large", "db.t3.medium")
	event.WorkloadID = wlID
	apply := event.CreatedAt
	seedNoErrors(t, st, wlID, apply.Add(-24*time.Hour), apply, apply, time.Now().UTC(),
		10, 10, 300, 300)

	var rollbacks []int64
	v := verifyDB(t, st, &rollbacks, event)
	if v.Failed {
		t.Fatalf("missing db_errors must never FAIL a healthy verification: %+v", v)
	}
	if v.Inconclusive {
		t.Fatalf("cpu + connections have data; the verdict should be passed: %+v", v)
	}
	if len(rollbacks) != 0 {
		t.Fatalf("no rollback for a passed verdict: %v", rollbacks)
	}
	runs, _ := st.ListVerificationRuns(context.Background(), nil)
	if len(runs) != 1 || runs[0].Verdict != store.VerdictPassed {
		t.Fatalf("want 1 passed run, got %+v", runs)
	}
	errs, ok := runs[0].SLIs["errors"].(map[string]any)
	if !ok {
		t.Fatalf("errors SLI evidence missing: %+v", runs[0].SLIs)
	}
	if errs["verdict"] != "unavailable" {
		t.Fatalf("missing metric must be no-evidence (unavailable), got: %+v", errs)
	}
}

// TestVerifyDBMissingErrorsDoesNotMaskFail: the same no-evidence rule
// must not hide a genuine regression — a CPU saturation FAIL still rolls
// back when db_errors is absent entirely.
func TestVerifyDBMissingErrorsDoesNotMaskFail(t *testing.T) {
	st := store.NewMemory()
	wlID, _ := st.UpsertWorkload(context.Background(), store.Workload{
		Name: "bursty-live", Namespace: "db", Kind: "database", Source: "db",
		DBClass: "db.t3.large",
	})
	event := dbEvent("db.t3.large", "db.t3.medium")
	event.WorkloadID = wlID
	apply := event.CreatedAt
	postEnd := apply.Add(2*time.Hour + 10*time.Minute)
	seedNoErrors(t, st, wlID, apply.Add(-24*time.Hour), apply, apply, postEnd,
		10, 80, 300, 300)

	var rollbacks []int64
	v := verifyDB(t, st, &rollbacks, event)
	if !v.Failed {
		t.Fatalf("cpu saturation must still FAIL without an errors metric: %+v", v)
	}
	if len(rollbacks) != 1 || rollbacks[0] != event.ID {
		t.Fatalf("auto-rollback on FAIL: %v", rollbacks)
	}
}

// TestVerifyRoutesClassEventsToDBPath proves the dispatch: a class
// event goes through VerifyDB with a nil Prometheus client (the k8s
// path would panic on it) and the verdict comes from store buckets.
func TestVerifyRoutesClassEventsToDBPath(t *testing.T) {
	st := store.NewMemory()
	wlID, _ := st.UpsertWorkload(context.Background(), store.Workload{
		Name: "payments-prod", Namespace: "db", Kind: "database", Source: "db",
		DBClass: "db.t3.large",
	})
	event := dbEvent("db.t3.large", "db.t3.medium")
	event.WorkloadID = wlID
	apply := event.CreatedAt
	seedDBMetrics(t, st, wlID, apply.Add(-24*time.Hour), apply, apply, time.Now().UTC(),
		10, 10, 300, 300, 5, 5)

	s := New(nil, st, func(_ context.Context, e store.ApplyEvent) error { return nil }, DefaultConfig())
	v, err := s.Verify(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if v.Failed || v.Inconclusive {
		t.Fatalf("class event via Verify must take the DB path, got %+v", v)
	}
}
