package api

import (
	"context"
	"fmt"
	"time"

	"consize/internal/analysis"
	"consize/internal/dbapply"
	"consize/internal/store"
)

// Recommendation risk levels (M3.5): low / medium / high, computed from
// existing data only — no schema change. Reasons name the triggers.
const (
	riskLow    = "low"
	riskMedium = "medium"
	riskHigh   = "high"
)

// recommendationView is the API's additive recommendation shape: the
// store recommendation plus per-recommendation risk.
type recommendationView struct {
	store.Recommendation
	Risk        string   `json:"risk"`
	RiskReasons []string `json:"risk_reasons"`
}

// riskWindow is how far back risk checks look (the analysis window).
const riskWindow = 14 * 24 * time.Hour

// recRisk computes one recommendation's risk from existing data only:
// workload labels (data-loss-risk), data days over the trailing window,
// and for class recommendations the step distance, maintenance-window
// state, pending follow-up, and saturation near headroom caps; for
// cpu/memory recommendations the utilization near the request line.
func (s *Server) recRisk(ctx context.Context, rec store.Recommendation, cache map[string][]store.Bucket) (string, []string) {
	wl, err := s.store.GetWorkload(ctx, rec.WorkloadID)
	if err != nil {
		// Defensive: ListRecommendations joins the workload, so this
		// should not happen; never crash the list over one bad row.
		return riskMedium, []string{"workload not found"}
	}

	var metrics []string
	if rec.Resource == store.ResourceClass {
		metrics = []string{
			store.MetricDBCPUPercent, store.MetricDBIOPS,
			store.MetricDBConnections, store.MetricDBMemPercent,
		}
	} else {
		metrics = []string{store.MetricCPUMilli, store.MetricMemBytes}
	}
	end := time.Now().UTC()
	start := end.Add(-riskWindow)
	buckets := map[string][]store.Bucket{}
	for _, m := range metrics {
		key := fmt.Sprintf("%d|%s", rec.WorkloadID, m)
		bs, ok := cache[key]
		if !ok {
			bs, _ = s.store.ListBuckets(ctx, rec.WorkloadID, m, start, end)
			cache[key] = bs
		}
		buckets[m] = bs
	}
	return riskFor(rec, wl, buckets)
}

// riskFor is the pure decision: risk level and reasons.
func riskFor(rec store.Recommendation, wl store.Workload, buckets map[string][]store.Bucket) (string, []string) {
	level := riskLow
	var reasons []string
	flag := func(lvl, reason string) {
		reasons = append(reasons, reason)
		if sev(lvl) > sev(level) {
			level = lvl
		}
	}

	if wl.Labels["consize.savings.dev/data-loss-risk"] == "true" {
		flag(riskHigh, "workload flagged data-loss-risk")
	}
	if d := daysWithData(buckets); d < analysis.MinDataDays {
		flag(riskMedium, fmt.Sprintf("low data days (%d of %d required)", d, analysis.MinDataDays))
	}

	if rec.Resource != store.ResourceClass {
		// k8s cpu/memory recommendations: utilization near the request
		// line (the 1.2x headroom boundary sits at 83% of the request).
		metric, dim := "", ""
		switch rec.Resource {
		case store.ResourceCPU:
			metric, dim = store.MetricCPUMilli, "cpu"
		case store.ResourceMemory:
			metric, dim = store.MetricMemBytes, "memory"
		}
		if metric != "" && rec.CurrentValue > 0 {
			if p95 := windowP95(buckets[metric]); p95 > 0 {
				if util := p95 / float64(rec.CurrentValue) * 100; util >= 80 {
					flag(riskMedium, fmt.Sprintf(
						"%s utilization p95 at %.1f%% of the current request (the 1.2x headroom boundary is 83%%)",
						dim, util))
				}
			}
		}
		return level, reasons
	}

	// Class recommendation.
	cur, curOK := analysis.DBClassByName(rec.ClassCurrent)
	if curOK {
		i, j := analysis.DBClassIndex(rec.ClassCurrent), analysis.DBClassIndex(rec.ClassProposed)
		if i >= 0 && j >= 0 && i-j > 1 {
			flag(riskMedium, fmt.Sprintf(
				"class step spans %d catalog classes (applies move one adjacent step at a time)", i-j))
		}
	}
	switch {
	case wl.DBMaintenanceWindow == "":
		flag(riskMedium, "no maintenance window configured (class applies are blocked)")
	default:
		if inWin, err := dbapply.InMaintenanceWindow(wl.DBMaintenanceWindow, time.Now().UTC()); err != nil {
			flag(riskHigh, "malformed maintenance window: "+err.Error())
		} else if !inWin {
			flag(riskMedium, "maintenance window not yet open")
		}
	}
	// A pending class recommendation whose current class is NOT the
	// instance's live class is the queued continuation of a stepped
	// apply (dbapply queues follow-ups with the just-applied class).
	if rec.Status == store.StatusPending && wl.DBClass != "" && rec.ClassCurrent != wl.DBClass {
		flag(riskMedium, "pending follow-up of a multi-step class plan (its current class is not the instance's live class)")
	}
	if curOK {
		if r := nearHeadroom("cpu", windowP95(buckets[store.MetricDBCPUPercent]), analysis.DBCPUCap, 0); r != "" {
			flag(riskMedium, r)
		}
		if r := nearHeadroom("iops", windowP95(buckets[store.MetricDBIOPS]), analysis.DBIOPSCap, float64(cur.MaxIOPS)); r != "" {
			flag(riskMedium, r)
		}
		if r := nearHeadroom("memory", windowP95(buckets[store.MetricDBMemPercent]), analysis.DBMemCap, 0); r != "" {
			flag(riskMedium, r)
		}
		if r := nearHeadroom("connections", windowP95(buckets[store.MetricDBConnections]), analysis.DBConnsCap, float64(cur.MaxConns)); r != "" {
			flag(riskMedium, r)
		}
	}
	return level, reasons
}

// nearHeadroom reports "near the cap" when a window p95 sits within 10
// points of its cap. p95 is a percent already; scale (> 0) converts
// absolute counts (IOPS, connections) into percents of a class baseline.
func nearHeadroom(dim string, p95, cap, scale float64) string {
	if scale > 0 {
		p95 = p95 / scale * 100
	}
	if p95 <= 0 || p95 < cap-10 {
		return ""
	}
	return fmt.Sprintf("%s p95 at %.1f%% within 10 points of the %.0f%% headroom cap", dim, p95, cap)
}

// daysWithData counts distinct UTC days with at least one bucket across
// all the metrics fetched for the recommendation.
func daysWithData(byMetric map[string][]store.Bucket) int {
	seen := map[int64]bool{}
	for _, bs := range byMetric {
		for _, b := range bs {
			seen[b.WindowStart.Unix()/86400] = true
		}
	}
	return len(seen)
}

// windowP95 is the analysis engine's window aggregation: the p95 of each
// day's p95s, then the p95 over the daily p95s. Empty data → 0.
func windowP95(bs []store.Bucket) float64 {
	byDay := map[int64][]float64{}
	for _, b := range bs {
		day := b.WindowStart.Unix() / 86400
		byDay[day] = append(byDay[day], b.P95)
	}
	var daily []float64
	for _, vals := range byDay {
		daily = append(daily, analysis.Percentile(vals, 95))
	}
	return analysis.Percentile(daily, 95)
}

func sev(l string) int {
	switch l {
	case riskHigh:
		return 3
	case riskMedium:
		return 2
	default:
		return 1
	}
}
