package main

import (
	"context"
	"testing"
	"time"

	"consize/internal/analysis"
	"consize/internal/pricing"
	"consize/internal/store"
)

func TestMergeBucketsJoinsOnWindow(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	step := 15 * time.Minute
	ts := func(i int) time.Time { return base.Add(time.Duration(i) * step) }

	mk := func(metric string, i int, p95 float64) store.Bucket {
		return store.Bucket{WorkloadID: 1, Metric: metric,
			WindowStart: ts(i), P95: p95}
	}

	cpu := []store.Bucket{
		mk(store.MetricCPUMilli, 0, 100),
		mk(store.MetricCPUMilli, 1, 200),
		mk(store.MetricCPUMilli, 2, 300), // window 2 has NO mem row
		mk(store.MetricCPUMilli, 4, 400), // window 4 has no mem row either
	}
	mem := []store.Bucket{
		mk(store.MetricMemBytes, 0, 1<<20),
		mk(store.MetricMemBytes, 1, 2<<20),
		mk(store.MetricMemBytes, 3, 3<<20),
	}

	got := mergeBuckets(cpu, mem)
	// Intersection: windows 0 and 1 only. A half-known window must not
	// drag percentiles toward zero.
	if len(got) != 2 {
		t.Fatalf("want 2 merged windows, got %d: %+v", len(got), got)
	}
	if got[0].CPUUsedMilli != 100 || got[0].MemUsedBytes != 1<<20 {
		t.Fatalf("window 0: %+v", got[0])
	}
	if got[1].CPUUsedMilli != 200 || got[1].MemUsedBytes != 2<<20 {
		t.Fatalf("window 1: %+v", got[1])
	}
}

// TestAnalyzeCommandPipeline runs the same flow as main: store buckets →
// analysis → recommendations persisted, superseding prior pending ones.
func TestAnalyzeCommandPipeline(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	wid, err := st.UpsertWorkload(ctx, store.Workload{
		Name: "api", Namespace: "prod", Source: "k8s",
		RequestCPUMilli: 240, LimitCPUMilli: 2000, // request == 200×1.2 → CPU already optimal
		RequestMemBytes: 1 << 30, LimitMemBytes: 2 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 5 days of steady usage at 200 milli / 300 MiB — enough data to
	// size memory down (300 MiB → 360 MiB); CPU stays put.
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for d := 0; d < 5; d++ {
		for i := 0; i < 96; i++ {
			ts := base.Add(time.Duration(d*96+i) * 15 * time.Minute)
			if err := st.UpsertBucket(ctx, store.Bucket{
				WorkloadID: wid, Metric: store.MetricCPUMilli, WindowStart: ts,
				P95: 200, P99: 220, Max: 250, Samples: 2,
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.UpsertBucket(ctx, store.Bucket{
				WorkloadID: wid, Metric: store.MetricMemBytes, WindowStart: ts,
				P95: 300 * 1 << 20, P99: 320 * 1 << 20, Max: 400 * 1 << 20, Samples: 2,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Run analysis twice: the second run must supersede the first's
	// pending recommendation, not duplicate it.
	for run := 0; run < 2; run++ {
		if _, _, _, _, err := analyzeAll(ctx, st, pricing.DefaultStatic(), analysis.DefaultConfig()); err != nil {
			t.Fatal(err)
		}
	}

	recs, _, err := st.ListRecommendations(ctx, &wid, store.StatusPending, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want exactly 1 pending recommendation, got %d: %+v", len(recs), recs)
	}
	if recs[0].Resource != store.ResourceMemory {
		t.Fatalf("resource: %s", recs[0].Resource)
	}
	if recs[0].CurrentValue != 1<<30 || recs[0].ProposedValue != 360<<20 {
		t.Fatalf("memory sizing: %+v", recs[0])
	}
	if recs[0].Confidence != 5.0/14.0 {
		t.Fatalf("confidence should be days/14: %v", recs[0].Confidence)
	}

	superseded, _, err := st.ListRecommendations(ctx, &wid, store.StatusSuperseded, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(superseded) != 1 {
		t.Fatalf("want 1 superseded, got %d", len(superseded))
	}
}

// TestAnalyzeCommandPipelineDB is the M3 seeded-RDS acceptance case
// (plan AC 1): a source="db" workload with the ADR-030 golden
// utilization (t3.large at cpu 10%, mem 12.5%, 200 IOPS, 300
// connections) must come out of the full analyze pipeline as
// db.t3.large → db.t3.medium at $50/mo — and must never enter the k8s
// policy (its zero requests/limits would otherwise look "already
// optimal").
func TestAnalyzeCommandPipelineDB(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	wid, err := st.UpsertWorkload(ctx, store.Workload{
		Name: "payments-prod", Namespace: "db", Kind: "database",
		Source:              "db",
		DBClass:             "db.t3.large",
		DBReplicas:          1,
		DBMaintenanceWindow: "mon:02:00-mon:03:00",
		DBProvider:          "aws",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 14 days of steady usage at the golden values.
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	metrics := []struct {
		name  string
		value float64
	}{
		{store.MetricDBCPUPercent, 10},
		{store.MetricDBMemPercent, 12.5},
		{store.MetricDBIOPS, 200},
		{store.MetricDBConnections, 300},
	}
	for d := 0; d < 14; d++ {
		for _, m := range metrics {
			if err := st.UpsertBucket(ctx, store.Bucket{
				WorkloadID: wid, Metric: m.name,
				WindowStart: base.Add(time.Duration(d) * 24 * time.Hour),
				P95:         m.value, P99: m.value, Max: m.value, Samples: 96,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	if _, _, _, _, err := analyzeAll(ctx, st, pricing.DefaultStatic(), analysis.DefaultConfig()); err != nil {
		t.Fatal(err)
	}

	recs, _, err := st.ListRecommendations(ctx, &wid, store.StatusPending, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want exactly 1 pending class recommendation, got %d: %+v", len(recs), recs)
	}
	r := recs[0]
	if r.Resource != store.ResourceClass {
		t.Fatalf("resource: want class, got %s", r.Resource)
	}
	if r.ClassCurrent != "db.t3.large" || r.ClassProposed != "db.t3.medium" {
		t.Fatalf("class pair: want db.t3.large → db.t3.medium, got %s → %s", r.ClassCurrent, r.ClassProposed)
	}
	if r.SavingsMonthly != 50 {
		t.Fatalf("savings: want $50/mo, got %v", r.SavingsMonthly)
	}
	if r.Confidence != 1.0 {
		t.Fatalf("confidence: want 1.0 from 14 days, got %v", r.Confidence)
	}
}

// TestAnalyzeCommandKeepWithRationale: a saturated DB instance produces
// no recommendation — the pipeline reports the keep with its bottleneck
// attribution and persists nothing.
func TestAnalyzeCommandKeepWithRationale(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	wid, err := st.UpsertWorkload(ctx, store.Workload{
		Name: "bursty", Namespace: "db", Kind: "database",
		Source:  "db",
		DBClass: "db.t3.large",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for d := 0; d < 14; d++ {
		for _, m := range []struct {
			name  string
			value float64
		}{
			{store.MetricDBCPUPercent, 90},
			{store.MetricDBMemPercent, 10},
			{store.MetricDBIOPS, 100},
			{store.MetricDBConnections, 100},
		} {
			if err := st.UpsertBucket(ctx, store.Bucket{
				WorkloadID: wid, Metric: m.name,
				WindowStart: base.Add(time.Duration(d) * 24 * time.Hour),
				P95:         m.value, P99: m.value, Max: m.value, Samples: 96,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	_, _, kept, _, err := analyzeAll(ctx, st, pricing.DefaultStatic(), analysis.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].Bottleneck != "cpu" || kept[0].ClassCurrent != "db.t3.large" {
		t.Fatalf("want keep with cpu bottleneck on db.t3.large, got %+v", kept)
	}

	recs, _, err := st.ListRecommendations(ctx, &wid, store.StatusPending, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("want no persisted recommendation for a kept instance, got %d: %+v", len(recs), recs)
	}
}
