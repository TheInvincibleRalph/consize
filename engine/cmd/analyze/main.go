// Command analyze runs the sizing policy over everything in the store
// and persists new recommendations (superseding prior pending ones).
//
// Pipeline: store buckets → analysis.Workload → analysis.Analyze
// → store.CreateRecommendations. Pure policy, no cluster access —
// it only needs the store and a price source.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"consize/internal/analysis"
	"consize/internal/config"
	"consize/internal/pricing"
	"consize/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, true)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	// Price source: static default, or AWS Price List via the resilient
	// wrapper so an unreachable AWS API degrades to the static table.
	var src pricing.Service = pricing.Static{P: pricing.DefaultStatic()}
	if config.Str("CONSIZE_PRICING", "static") == "aws" {
		aws := pricing.NewAWS(config.Str("CONSIZE_REGION", "us-east-1"))
		src = pricing.NewResilient(pricing.NewCached(aws, 24*time.Hour), pricing.DefaultStatic())
	}
	prices, err := src.Prices(ctx)
	if err != nil {
		log.Fatalf("prices: %v", err)
	}

	// Policy config: the data-minimum is a confidence gate, not a safety
	// gate (ADR-024) — operators of new fleets may lower it; the verifier
	// still protects every apply.
	cfg := analysis.DefaultConfig()
	cfg.MinDataDays = config.Float("CONSIZE_MIN_DATA_DAYS", analysis.MinDataDays)

	k8sRecs, dbRecs, kept, skipped, err := analyzeAll(ctx, st, prices, cfg)
	if err != nil {
		log.Fatalf("analyze: %v", err)
	}

	var total float64
	for _, r := range k8sRecs {
		total += r.SavingsMonth
		fmt.Printf("%-40s %-6s current=%9d proposed=%9d  $%8.2f/mo  confidence=%.0f%%\n",
			r.Namespace+"/"+r.Workload, r.Resource, r.Current, r.Recommended, r.SavingsMonth, r.Confidence*100)
	}
	for _, r := range dbRecs {
		total += r.SavingsMonth
		fmt.Printf("%-40s %-6s %-12s → %-12s $%8.2f/mo  confidence=%.0f%%\n",
			r.Namespace+"/"+r.Workload, "class", r.ClassCurrent, r.ClassProposed, r.SavingsMonth, r.Confidence*100)
	}
	for _, k := range kept {
		fmt.Printf("%-40s %-6s %s kept — bottleneck %s (headroom cap exceeded)\n",
			k.Namespace+"/"+k.Workload, "keep", k.ClassCurrent, k.Bottleneck)
	}
	for _, s := range skipped {
		slog.Debug("skipped", "workload", s.Namespace+"/"+s.Workload, "reason", s.Reason)
	}
	fmt.Printf("\nTOTAL PROJECTED: $%.2f / month across %d recommendations\n", total, len(k8sRecs)+len(dbRecs))
}

// analyzeAll runs the full pipeline against a store: load workloads and
// buckets, run the sizing policy per surface (k8s workloads → CPU/memory
// policy; source="db" workloads → class policy, ADR-030), persist
// recommendations. Pure enough to test with the in-memory store.
func analyzeAll(ctx context.Context, st store.Store, prices analysis.Prices, cfg analysis.Config) ([]analysis.Recommendation, []analysis.DBRecommendation, []analysis.DBKeep, []analysis.Skipped, error) {
	workloads, err := st.ListWorkloads(ctx)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("list workloads: %w", err)
	}

	type engineWorkload struct {
		analysis.Workload
		id int64
	}
	var dbInstances []analysis.DBInstance
	engine := make([]engineWorkload, 0, len(workloads))
	for _, w := range workloads {
		if w.Source == "db" {
			inst, err := dbInstanceFromStore(ctx, st, w)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			dbInstances = append(dbInstances, inst)
			continue
		}
		cpu, err := st.ListBuckets(ctx, w.ID, store.MetricCPUMilli, time.Time{}, time.Now().Add(24*time.Hour))
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("buckets cpu %s/%s: %w", w.Namespace, w.Name, err)
		}
		mem, err := st.ListBuckets(ctx, w.ID, store.MetricMemBytes, time.Time{}, time.Now().Add(24*time.Hour))
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("buckets mem %s/%s: %w", w.Namespace, w.Name, err)
		}
		engine = append(engine, engineWorkload{
			Workload: analysis.Workload{
				Name:       w.Name,
				Namespace:  w.Namespace,
				Kind:       w.Kind,
				Labels:     w.Labels,
				RequestCPU: w.RequestCPUMilli,
				LimitCPU:   w.LimitCPUMilli,
				RequestMem: w.RequestMemBytes,
				LimitMem:   w.LimitMemBytes,
				Buckets:    mergeBuckets(cpu, mem),
			},
			id: w.ID,
		})
	}

	ws := make([]analysis.Workload, len(engine))
	for i := range engine {
		ws[i] = engine[i].Workload
	}
	res := analysis.AnalyzeCfg(ws, prices, cfg)
	dbRes := analysis.DBAnalyzeCfg(dbInstances, cfg)

	// Persist k8s recommendations (supersede prior pending).
	recs := make([]store.Recommendation, 0, len(res.Recommendations))
	for _, r := range res.Recommendations {
		var wid int64
		for i := range engine {
			if engine[i].Name == r.Workload && engine[i].Namespace == r.Namespace {
				wid = engine[i].id
				break
			}
		}
		recs = append(recs, store.Recommendation{
			WorkloadID:     wid,
			Resource:       r.Resource,
			CurrentValue:   r.Current,
			ProposedValue:  r.Recommended,
			CurrentLimit:   r.LimitCurrent,
			ProposedLimit:  r.LimitProposed,
			SavingsMonthly: r.SavingsMonth,
			Confidence:     r.Confidence,
			PolicyVersion:  "v1",
			Status:         store.StatusPending,
		})
	}
	if err := st.CreateRecommendations(ctx, recs); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("persist recommendations: %w", err)
	}

	// Persist DB class recommendations (ADR-030): Resource="class", the
	// class pair instead of byte values. Kept/skipped instances are
	// output-only (the "keep with rationale" report) — nothing to persist.
	widByName := make(map[string]int64, len(workloads))
	for _, w := range workloads {
		widByName[w.Namespace+"/"+w.Name] = w.ID
	}
	dbRecs := make([]store.Recommendation, 0, len(dbRes.Recommendations))
	for _, r := range dbRes.Recommendations {
		wid, ok := widByName[r.Namespace+"/"+r.Workload]
		if !ok {
			return nil, nil, nil, nil, fmt.Errorf("db recommendation for unknown workload %s/%s", r.Namespace, r.Workload)
		}
		dbRecs = append(dbRecs, store.Recommendation{
			WorkloadID:     wid,
			Resource:       store.ResourceClass,
			ClassCurrent:   r.ClassCurrent,
			ClassProposed:  r.ClassProposed,
			SavingsMonthly: r.SavingsMonth,
			Confidence:     r.Confidence,
			PolicyVersion:  "v1",
			Status:         store.StatusPending,
		})
	}
	if err := st.CreateRecommendations(ctx, dbRecs); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("persist db recommendations: %w", err)
	}

	// Housekeeping: superseded recommendations are replaced by construction
	// (CreateRecommendations supersedes the pending row), so old ones are
	// pure history. The audit of what was *applied* lives in apply_events
	// and verification_runs, which are never pruned. CONSIZE_REC_RETENTION
	// defaults to 7 days — long enough to see the recent recommendation
	// history for a workload, short enough to keep list responses bounded.
	pruned, err := st.PruneRecommendations(ctx, store.StatusSuperseded, time.Now().UTC().Add(-config.Duration("CONSIZE_REC_RETENTION", 168*time.Hour)))
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("prune superseded recommendations: %w", err)
	}
	if pruned > 0 {
		slog.Info("pruned superseded recommendations", "count", pruned)
	}

	sort.Slice(res.Recommendations, func(i, j int) bool {
		return res.Recommendations[i].SavingsMonth > res.Recommendations[j].SavingsMonth
	})
	skipped := append([]analysis.Skipped{}, res.Skipped...)
	skipped = append(skipped, dbRes.Skipped...)
	return res.Recommendations, dbRes.Recommendations, dbRes.Kept, skipped, nil
}

// dbInstanceFromStore loads one database's usage buckets and maps them
// into the policy's input shape. The store metric names are the ADR-030
// names the policy consumes verbatim (db_cpu_percent, db_mem_percent,
// db_iops, db_connections); db_errors is a verifier SLI and is not part
// of sizing.
func dbInstanceFromStore(ctx context.Context, st store.Store, w store.Workload) (analysis.DBInstance, error) {
	inst := analysis.DBInstance{Name: w.Name, Namespace: w.Namespace, Class: w.DBClass}
	for _, m := range []string{store.MetricDBCPUPercent, store.MetricDBMemPercent, store.MetricDBIOPS, store.MetricDBConnections} {
		bs, err := st.ListBuckets(ctx, w.ID, m, time.Time{}, time.Now().Add(24*time.Hour))
		if err != nil {
			return analysis.DBInstance{}, fmt.Errorf("buckets %s %s/%s: %w", m, w.Namespace, w.Name, err)
		}
		for _, b := range bs {
			inst.Buckets = append(inst.Buckets, analysis.DBBucket{
				Metric:      m,
				WindowStart: b.WindowStart.Unix(),
				Value:       b.P95,
			})
		}
	}
	return inst, nil
}

// mergeBuckets joins the CPU and memory bucket series on window_start.
// A window present in only one series is dropped: a half-known window
// would drag the other metric's percentiles toward zero and could
// fabricate "already optimal". Both queries run at the same steps, so
// in practice every window has both rows.
func mergeBuckets(cpu, mem []store.Bucket) []analysis.Bucket {
	memByTS := make(map[int64]float64, len(mem))
	for _, b := range mem {
		memByTS[b.WindowStart.Unix()] = b.P95
	}
	out := make([]analysis.Bucket, 0, len(cpu))
	for _, b := range cpu {
		m, ok := memByTS[b.WindowStart.Unix()]
		if !ok {
			continue
		}
		out = append(out, analysis.Bucket{
			CPUUsedMilli: int64(b.P95),
			MemUsedBytes: int64(m),
			WindowStart:  b.WindowStart.Unix(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WindowStart < out[j].WindowStart })
	return out
}
