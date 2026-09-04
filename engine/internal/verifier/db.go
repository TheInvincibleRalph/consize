// DB verification (ADR-030 §7): SLIs come from the store's
// usage_buckets — the collector ingests DB metrics, the verifier reads
// them back; there is no live-provider query on the verify path.
//
// Judgment is against the ANALYSIS CAPS, not baseline-relative
// multipliers: a class downsize legitimately raises utilization, so a
// relative threshold would false-positive on every healthy downsize.
// A regression is sustained utilization at or above the cap the
// analysis promised to keep:
//
//	cpu_saturation  db_cpu_percent   sustained ≥ 60% (DBCPUCap)
//	connections     db_connections   sustained ≥ 70% of the NEW class
//	                                  baseline (DBConnsCap) — values are
//	                                  absolute counts, projected onto the
//	                                  class that was actually applied
//	errors          db_errors        more errors in the post window than
//	                                  the baseline (per-window counts;
//	                                  the collector's ingestion contract)
//
// Buckets are 15-minute steps, so "sustained" is measured in 15-minute
// units: one bucket at/above a cap is a breach (15m ≥ the shipped
// 5-minute default), two consecutive buckets are a 30m breach. The
// verification_run records the same evidence shape as the k8s path.
package verifier

import (
	"context"
	"fmt"
	"time"

	"consize/internal/analysis"
	"consize/internal/config"
	"consize/internal/store"
)

// DBStepMinutes is the collector's step for DB metrics — the resolution
// of a verification "sample".
const DBStepMinutes = 15

// VerifyDB judges one applied class event against the store buckets and
// shares the record/alert/rollback tail with the k8s path. The rollback
// func was wired by the caller to the DB apply engine's Rollback — the
// verifier never touches the provider itself.
func (s *Service) VerifyDB(ctx context.Context, event store.ApplyEvent) (Verdict, error) {
	wl, err := s.st.GetWorkload(ctx, event.WorkloadID)
	if err != nil {
		return Verdict{}, fmt.Errorf("load workload %d: %w", event.WorkloadID, err)
	}
	if event.Diff.Resource != store.ResourceClass || event.Diff.ClassProposed == "" {
		return Verdict{}, fmt.Errorf("event %d is not a class event (resource=%q proposed=%q)",
			event.ID, event.Diff.Resource, event.Diff.ClassProposed)
	}

	applyTime := event.CreatedAt.UTC()
	window := config.StepScaledDuration(s.cfg.Window, event.StepNumber)
	baseStart, baseEnd := applyTime.Add(-window), applyTime
	postStart, postEnd := applyTime, time.Now().UTC()

	v := Verdict{SLIs: map[string]any{}, Thresholds: map[string]any{}}
	var judged, inconclusive int
	for _, sig := range s.dbSignals(event) {
		ev, err := s.dbEvaluate(ctx, sig, wl.ID, baseStart, baseEnd, postStart, postEnd)
		if err != nil {
			return v, err
		}
		v.SLIs[sig.name] = ev
		switch ev["verdict"] {
		case "failed":
			v.Failed = true
			judged++
		case "inconclusive":
			inconclusive++
			v.Inconclusive = true
		case "passed":
			judged++
		default: // unavailable — no DB metrics ingested
			// never silence: falls through to the judged==0 rule below
		}
	}
	// Never silence: a verification where nothing could be judged is
	// inconclusive, not a pass (ADR-006).
	if judged == 0 && inconclusive == 0 {
		v.Inconclusive = true
	}
	v.Thresholds = map[string]any{
		"window":            window.String(),
		"base_window":       s.cfg.Window.String(),
		"step_number":       event.StepNumber,
		"sustained_minutes": s.cfg.SustainedMinutes,
		"db_step_minutes":   DBStepMinutes,
		"cpu_cap_percent":   analysis.DBCPUCap,
		"conns_cap_percent": analysis.DBConnsCap,
		"connections_class": event.Diff.ClassProposed,
	}

	return v, s.recordAndAct(ctx, event, wl, v, baseStart, baseEnd, postStart, postEnd)
}

// dbSignalSpec is one DB SLI: which store metric, how to judge it.
type dbSignalSpec struct {
	name   string
	metric string // store.MetricDB*
	kind   string // "rate" | "counter"
	cap    float64
	class  string // new class for the connections projection
}

func (s *Service) dbSignals(event store.ApplyEvent) []dbSignalSpec {
	return []dbSignalSpec{
		{name: "cpu_saturation", metric: store.MetricDBCPUPercent, kind: "rate", cap: analysis.DBCPUCap},
		{name: "connections", metric: store.MetricDBConnections, kind: "rate", cap: analysis.DBConnsCap, class: event.Diff.ClassProposed},
		{name: "errors", metric: store.MetricDBErrors, kind: "counter"},
	}
}

// dbEvaluate judges one signal from store buckets and returns its
// evidence map. ok=false means the metric was never ingested.
func (s *Service) dbEvaluate(ctx context.Context, sig dbSignalSpec, workloadID int64,
	baseStart, baseEnd, postStart, postEnd time.Time) (map[string]any, error) {

	ev := map[string]any{"signal": sig.name}
	base, err := s.dbFetch(ctx, workloadID, sig.metric, baseStart, baseEnd)
	if err != nil {
		return nil, err
	}
	post, err := s.dbFetch(ctx, workloadID, sig.metric, postStart, postEnd)
	if err != nil {
		return nil, err
	}
	if len(base) == 0 && len(post) == 0 {
		ev["verdict"] = "unavailable"
		return ev, nil
	}
	if len(base) == 0 || len(post) == 0 {
		ev["verdict"] = "inconclusive"
		ev["reason"] = "data missing in one window"
		return ev, nil
	}

	switch sig.kind {
	case "rate":
		postVals, baseVals := post, base
		if sig.class != "" {
			cls, ok := dbClassFor(sig.class)
			if !ok {
				ev["verdict"] = "inconclusive"
				ev["reason"] = "unknown class " + sig.class
				return ev, nil
			}
			// Connections are absolute counts; project onto the applied
			// class's baseline to get utilization percents.
			postVals = projectConns(post, cls.MaxConns)
			baseVals = projectConns(base, cls.MaxConns)
		}
		run := dbLongestRun(postVals, sig.cap)
		ev["baseline_median"] = median(baseVals)
		ev["threshold"] = sig.cap
		ev["post_above_threshold"] = run.samples
		ev["longest_breach_minutes"] = run.minutes * DBStepMinutes
		if run.minutes*DBStepMinutes >= s.cfg.SustainedMinutes {
			ev["verdict"] = "failed"
			ev["reason"] = fmt.Sprintf("%d buckets at/above %.4g%% sustained %d min",
				run.samples, sig.cap, run.minutes*DBStepMinutes)
		} else {
			ev["verdict"] = "passed"
		}
	case "counter":
		// db_errors is a per-window count, so totals across windows of
		// different lengths would be meaningless (a 24h baseline vs a
		// fresh post window). Judge the per-bucket rate: a sustained
		// rise in errors-per-window is the regression signature.
		baseMed, postMed := median(base), median(post)
		ev["baseline_median"], ev["post_median"] = baseMed, postMed
		if postMed > baseMed {
			ev["verdict"] = "failed"
			ev["reason"] = fmt.Sprintf("errors per window up: %.4g post vs %.4g baseline", postMed, baseMed)
		} else {
			ev["verdict"] = "passed"
		}
	}
	return ev, nil
}

// dbFetch returns one metric's p95 values over a window, in window
// order (the store guarantees ordering).
func (s *Service) dbFetch(ctx context.Context, workloadID int64, metric string, start, end time.Time) ([]float64, error) {
	bs, err := s.st.ListBuckets(ctx, workloadID, metric, start, end)
	if err != nil {
		return nil, err
	}
	out := make([]float64, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.P95)
	}
	return out, nil
}

// projectConns converts absolute connection counts into utilization
// percents of a class's baseline.
func projectConns(vals []float64, max int) []float64 {
	out := make([]float64, len(vals))
	for i, v := range vals {
		out[i] = v / float64(max) * 100
	}
	return out
}

// dbLongestRun is longestRun with ≥ semantics: the analysis promise is
// "projected p95 BELOW the cap", so a value exactly at the cap is a
// breach.
func dbLongestRun(values []float64, threshold float64) run {
	best, cur := 0, 0
	for _, v := range values {
		if v >= threshold {
			cur++
			if cur > best {
				best = cur
			}
		} else {
			cur = 0
		}
	}
	return run{samples: best, minutes: best} // one bucket per sample
}

// dbClassFor resolves a class across both provider catalogs (RDS and
// GCP Cloud SQL tiers).
func dbClassFor(name string) (analysis.DBClass, bool) {
	return analysis.DBClassByName(name)
}
