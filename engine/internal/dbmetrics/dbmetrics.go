// Package dbmetrics is the collector-side seam for database providers
// (ADR-030 §8). A Source lists instances and yields metric series; the
// collector upserts instances as Workloads (Source="db") and their
// series into usage_buckets with the db_* metric names. The verifier
// needs no provider interface at all — it reads the buckets back from
// the store.
//
// The shipped implementation is a deterministic Fixture (tests, live
// demo). Live CloudWatch (RDS) and Cloud Monitoring (Cloud SQL)
// adapters implement the same interface later, exactly as GCP pricing
// landed behind pricing.Service (M1).
package dbmetrics

import (
	"context"
	"math"
	"time"

	"consize/internal/store"
)

// Instance is one database instance as the collector sees it.
type Instance struct {
	Name              string
	Namespace         string // provider namespace, e.g. "rds"
	Class             string // db.t3.large
	Replicas          int
	MaintenanceWindow string // UTC ddd:hh:mm-ddd:hh:mm (dbapply semantics)
	Provider          string // aws | gcp | fixture
	Labels            map[string]string
}

// Source lists instances and yields p95 series for one metric over a
// window, step-aligned like the k8s Prometheus path.
type Source interface {
	ListInstances(ctx context.Context) ([]Instance, error)
	// Series returns one bucket per step-aligned window in [start, end)
	// for the given instance and metric. Metric names are the store's
	// MetricDB* constants. Single-sample windows: P50=P95=P99=Max.
	Series(ctx context.Context, inst Instance, metric string, start, end time.Time, step time.Duration) ([]store.Bucket, error)
}

// Fixture is a deterministic Source for tests and the live-cluster
// demo: one RDS-style instance running the hand-computed golden demand
// — 10% CPU, 12.5% memory (1 GiB of 8), 200 IOPS, 300 connections —
// modulated by daily/weekly sinusoids (±10% / ±5%). The charts look
// alive while the projected p95 stays within a few percent of the base,
// so the golden recommendation (db.t3.medium, $50/mo) holds exactly.
type Fixture struct {
	// Instances is the demo set. Default from NewFixture; tests may
	// replace it.
	Instances []Instance
	demand    map[string]float64 // metric → base value
}

// NewFixture returns the shipped demo fixture.
func NewFixture() *Fixture {
	return &Fixture{
		Instances: []Instance{{
			Name:              "payments-prod",
			Namespace:         "rds",
			Class:             "db.t3.large",
			Replicas:          1,
			MaintenanceWindow: "sun:00:00-sat:00:00", // only Saturday UTC blocked
			Provider:          "fixture",
			Labels: map[string]string{
				"app":                         "payments",
				"consize.savings.dev/auto-db": "enabled", // auto mode works out of the box
				"consize.savings.dev/exclude": "false",
			},
		}},
		demand: map[string]float64{
			store.MetricDBCPUPercent:  10,   // percent of vCPU
			store.MetricDBMemPercent:  12.5, // percent of class memory
			store.MetricDBIOPS:        200,  // absolute
			store.MetricDBConnections: 300,  // absolute
			store.MetricDBErrors:      2,    // per bucket — verifier's error SLI passes on a healthy downsize
		},
	}
}

// ListInstances returns the fixture's demo instances.
func (f *Fixture) ListInstances(context.Context) ([]Instance, error) {
	return append([]Instance(nil), f.Instances...), nil
}

// Series evaluates the demand model at each step-aligned window.
func (f *Fixture) Series(_ context.Context, inst Instance, metric string,
	start, end time.Time, step time.Duration) ([]store.Bucket, error) {

	base, ok := f.demand[metric]
	if !ok {
		return nil, nil // unknown metric: no data, like a source that doesn't emit it
	}
	start = start.UTC().Truncate(step)
	out := make([]store.Bucket, 0, int(end.Sub(start)/step)+1)
	for t := start; t.Before(end); t = t.Add(step) {
		v := f.demandValue(base, metric, t)
		out = append(out, store.Bucket{
			WorkloadID:  -1, // assigned by the collector
			Metric:      metric,
			WindowStart: t,
			P50:         v, P95: v, P99: v, Max: v,
			Samples: 1,
		})
	}
	return out, nil
}

// demandValue is the deterministic model: base modulated by a daily
// (±10%) and a weekly (±5%) sinusoid, evaluated from the Unix clock so
// re-collecting a window reproduces the same values. Pure counts round
// to integers; percents to one decimal.
func (f *Fixture) demandValue(base float64, metric string, t time.Time) float64 {
	secs := t.Unix()
	h := float64(secs%86400) / 86400
	w := float64(secs%(7*86400)) / (7 * 86400)
	v := base * (1 + 0.10*math.Sin(2*math.Pi*h) + 0.05*math.Sin(2*math.Pi*w))
	switch metric {
	case store.MetricDBCPUPercent, store.MetricDBMemPercent:
		return math.Round(v*10) / 10
	default:
		return math.Round(v)
	}
}
