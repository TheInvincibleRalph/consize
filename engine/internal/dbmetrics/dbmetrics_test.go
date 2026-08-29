package dbmetrics

import (
	"context"
	"testing"
	"time"

	"consize/internal/store"
)

func TestFixtureListInstances(t *testing.T) {
	f := NewFixture()
	insts, err := f.ListInstances(context.Background())
	if err != nil || len(insts) != 1 {
		t.Fatalf("want 1 instance, got %d err=%v", len(insts), err)
	}
	i := insts[0]
	if i.Name != "payments-prod" || i.Class != "db.t3.large" || i.Namespace != "rds" {
		t.Fatalf("instance: %+v", i)
	}
	if i.Provider != "fixture" || i.MaintenanceWindow == "" {
		t.Fatalf("provider/window: %+v", i)
	}
	if i.Labels["consize.savings.dev/auto-db"] != "enabled" {
		t.Fatalf("auto-db opt-in must be on for the demo: %v", i.Labels)
	}
}

// TestFixtureSeriesStepAligned: buckets land on the 15-minute grid over
// the requested window, with single-sample fields and plausible demand
// values inside the sinusoid bounds.
func TestFixtureSeriesStepAligned(t *testing.T) {
	f := NewFixture()
	inst := f.Instances[0]
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	bs, err := f.Series(context.Background(), inst, store.MetricDBCPUPercent,
		start.Add(3*time.Minute), start.Add(24*time.Hour), 15*time.Minute) // unaligned start
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 96 {
		t.Fatalf("24h at 15m steps: want 96 buckets, got %d", len(bs))
	}
	for _, b := range bs {
		if b.WindowStart.Unix()%900 != 0 {
			t.Fatalf("bucket not step-aligned: %s", b.WindowStart)
		}
		if b.P50 != b.P95 || b.P95 != b.P99 || b.P99 != b.Max {
			t.Fatalf("single-sample window expected: %+v", b)
		}
		if b.P95 < 8.5 || b.P95 > 11.5 { // base 10 ± 10% daily ± 5% weekly
			t.Fatalf("cpu demand out of range: %v", b.P95)
		}
	}
}

// TestFixtureSeriesExactAtSineZero: at a timestamp where both sinusoids
// are zero (epoch is a multiple of a day and a week), the value equals
// the base exactly — deterministic and hand-computable.
func TestFixtureSeriesExactAtSineZero(t *testing.T) {
	f := NewFixture()
	inst := f.Instances[0]
	start := time.Unix(0, 0).UTC()
	got := map[string]float64{}
	for _, m := range []string{store.MetricDBCPUPercent, store.MetricDBMemPercent,
		store.MetricDBIOPS, store.MetricDBConnections, store.MetricDBErrors} {
		bs, err := f.Series(context.Background(), inst, m, start, start.Add(15*time.Minute), 15*time.Minute)
		if err != nil || len(bs) != 1 {
			t.Fatalf("%s: %d buckets err=%v", m, len(bs), err)
		}
		got[m] = bs[0].P95
	}
	want := map[string]float64{
		store.MetricDBCPUPercent:  10,
		store.MetricDBMemPercent:  12.5,
		store.MetricDBIOPS:        200,
		store.MetricDBConnections: 300,
		store.MetricDBErrors:      2,
	}
	for m, w := range want {
		if got[m] != w {
			t.Fatalf("%s: want %v, got %v", m, w, got[m])
		}
	}
}

func TestFixtureSeriesUnknownMetricIsEmpty(t *testing.T) {
	f := NewFixture()
	bs, err := f.Series(context.Background(), f.Instances[0], "not_a_metric",
		time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC().Add(time.Hour), 15*time.Minute)
	if err != nil || len(bs) != 0 {
		t.Fatalf("unknown metric: %d buckets err=%v", len(bs), err)
	}
}
