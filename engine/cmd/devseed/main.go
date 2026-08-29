// Command devseed populates a local Consize store with fresh deterministic
// data. It exists for product development: the local dashboard should have
// realistic workloads, charts, recommendations, and savings without reading
// from the live GKE cluster.
package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"consize/internal/analysis"
	"consize/internal/config"
	"consize/internal/dbmetrics"
	"consize/internal/fixtures"
	"consize/internal/store"
)

func main() {
	ctx := context.Background()
	st, err := store.Open(ctx, true)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	step := config.Duration("CONSIZE_STEP", 15*time.Minute)
	window := config.Duration("CONSIZE_WINDOW", 14*24*time.Hour)
	end := time.Now().UTC().Truncate(step)
	start := end.Add(-window).Add(step)

	wids := map[string]int64{}
	k8s := freshWorkloads(fixtures.Workloads(), start)
	for _, w := range k8s {
		id, err := st.UpsertWorkload(ctx, store.Workload{
			Name:            w.Name,
			Namespace:       w.Namespace,
			Kind:            w.Kind,
			Labels:          w.Labels,
			RequestCPUMilli: w.RequestCPU,
			LimitCPUMilli:   w.LimitCPU,
			RequestMemBytes: w.RequestMem,
			LimitMemBytes:   w.LimitMem,
			Source:          "k8s",
		})
		if err != nil {
			log.Fatalf("upsert workload %s/%s: %v", w.Namespace, w.Name, err)
		}
		wids[w.Namespace+"/"+w.Name] = id
		for _, b := range w.Buckets {
			ts := time.Unix(b.WindowStart, 0).UTC()
			upsertBucket(ctx, st, store.Bucket{
				WorkloadID: id, Metric: store.MetricCPUMilli, WindowStart: ts,
				P50: float64(b.CPUUsedMilli), P95: float64(b.CPUUsedMilli),
				P99: float64(b.CPUUsedMilli), Max: float64(b.CPUUsedMilli), Samples: 1,
			})
			upsertBucket(ctx, st, store.Bucket{
				WorkloadID: id, Metric: store.MetricMemBytes, WindowStart: ts,
				P50: float64(b.MemUsedBytes), P95: float64(b.MemUsedBytes),
				P99: float64(b.MemUsedBytes), Max: float64(b.MemUsedBytes), Samples: 1,
			})
		}
	}

	dbInstances := seedDB(ctx, st, wids, start, end, step)

	cfg := analysis.DefaultConfig()
	cfg.MinDataDays = config.Float("CONSIZE_MIN_DATA_DAYS", 0.1)
	k8sResult := analysis.AnalyzeCfg(k8s, analysis.DefaultPrices(), cfg)
	dbResult := analysis.DBAnalyzeCfg(dbInstances, cfg)

	recs := make([]store.Recommendation, 0, len(k8sResult.Recommendations)+len(dbResult.Recommendations))
	for _, r := range k8sResult.Recommendations {
		wid := wids[r.Namespace+"/"+r.Workload]
		recs = append(recs, store.Recommendation{
			WorkloadID:     wid,
			Resource:       r.Resource,
			CurrentValue:   r.Current,
			ProposedValue:  r.Recommended,
			CurrentLimit:   r.LimitCurrent,
			ProposedLimit:  r.LimitProposed,
			SavingsMonthly: r.SavingsMonth,
			Confidence:     r.Confidence,
			PolicyVersion:  "dev-fixture",
			Status:         store.StatusPending,
		})
	}
	for _, r := range dbResult.Recommendations {
		wid := wids[r.Namespace+"/"+r.Workload]
		recs = append(recs, store.Recommendation{
			WorkloadID:     wid,
			Resource:       store.ResourceClass,
			ClassCurrent:   r.ClassCurrent,
			ClassProposed:  r.ClassProposed,
			SavingsMonthly: r.SavingsMonth,
			Confidence:     r.Confidence,
			PolicyVersion:  "dev-fixture",
			Status:         store.StatusPending,
		})
	}
	if err := st.CreateRecommendations(ctx, recs); err != nil {
		log.Fatalf("persist recommendations: %v", err)
	}

	var total float64
	for _, r := range recs {
		total += r.SavingsMonthly
	}
	fmt.Printf("seeded %d workloads, %d database instances, %d recommendations, $%.2f/mo projected\n",
		len(k8s), len(dbInstances), len(recs), total)
}

func freshWorkloads(in []analysis.Workload, start time.Time) []analysis.Workload {
	out := append([]analysis.Workload(nil), in...)
	for i := range out {
		sort.Slice(out[i].Buckets, func(a, b int) bool {
			return out[i].Buckets[a].WindowStart < out[i].Buckets[b].WindowStart
		})
		if len(out[i].Buckets) == 0 {
			continue
		}
		base := out[i].Buckets[0].WindowStart
		for j := range out[i].Buckets {
			offset := time.Duration(out[i].Buckets[j].WindowStart-base) * time.Second
			out[i].Buckets[j].WindowStart = start.Add(offset).Unix()
		}
	}
	return out
}

func seedDB(ctx context.Context, st store.Store, wids map[string]int64, start, end time.Time, step time.Duration) []analysis.DBInstance {
	src := dbmetrics.NewFixture()
	insts, err := src.ListInstances(ctx)
	if err != nil {
		log.Fatalf("list db fixtures: %v", err)
	}
	out := make([]analysis.DBInstance, 0, len(insts))
	for _, inst := range insts {
		id, err := st.UpsertWorkload(ctx, store.Workload{
			Name: inst.Name, Namespace: inst.Namespace, Kind: "database", Source: "db",
			Labels: inst.Labels, DBClass: inst.Class, DBReplicas: inst.Replicas,
			DBMaintenanceWindow: inst.MaintenanceWindow, DBProvider: inst.Provider,
		})
		if err != nil {
			log.Fatalf("upsert db instance %s/%s: %v", inst.Namespace, inst.Name, err)
		}
		wids[inst.Namespace+"/"+inst.Name] = id
		dbi := analysis.DBInstance{Name: inst.Name, Namespace: inst.Namespace, Class: inst.Class}
		for _, metric := range []string{
			store.MetricDBCPUPercent, store.MetricDBIOPS,
			store.MetricDBConnections, store.MetricDBMemPercent, store.MetricDBErrors,
		} {
			series, err := src.Series(ctx, inst, metric, start, end, step)
			if err != nil {
				log.Fatalf("db fixture series %s/%s: %v", inst.Name, metric, err)
			}
			for _, b := range series {
				b.WorkloadID = id
				upsertBucket(ctx, st, b)
				dbi.Buckets = append(dbi.Buckets, analysis.DBBucket{
					Metric:      metric,
					WindowStart: b.WindowStart.Unix(),
					Value:       b.P95,
				})
			}
		}
		out = append(out, dbi)
	}
	return out
}

func upsertBucket(ctx context.Context, st store.Store, b store.Bucket) {
	if err := st.UpsertBucket(ctx, b); err != nil {
		log.Fatalf("upsert bucket workload=%d metric=%s start=%s: %v",
			b.WorkloadID, b.Metric, b.WindowStart.Format(time.RFC3339), err)
	}
}
