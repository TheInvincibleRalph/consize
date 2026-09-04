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

	// Seed some history for the demo so the dashboard isn't completely blank
	seedHistory(ctx, st)

	// Seed cloud waste (cost opportunities)
	seedWaste(ctx, st)
}

func seedWaste(ctx context.Context, st store.Store) {
	now := time.Now().UTC()
	opps := []store.CostOpportunity{
		{
			Provider:       "aws",
			Account:        "production (123456789012)",
			Region:         "us-east-1",
			ResourceType:   "ebs",
			ResourceID:     "vol-0abcd1234efgh5678",
			Name:           "old-analytics-db-data",
			MonthlyCost:    120.00,
			Recommendation: "Delete unattached EBS volume",
			Action:         "delete",
			Risk:           "low",
			Status:         "open",
			FirstSeenAt:    now.Add(-30 * 24 * time.Hour),
			LastSeenAt:     now,
		},
		{
			Provider:       "aws",
			Account:        "staging (098765432109)",
			Region:         "us-west-2",
			ResourceType:   "elastic_ip",
			ResourceID:     "eipalloc-01234567",
			Name:           "34.210.X.X",
			MonthlyCost:    3.65,
			Recommendation: "Release unused Elastic IP",
			Action:         "release",
			Risk:           "low",
			Status:         "open",
			FirstSeenAt:    now.Add(-15 * 24 * time.Hour),
			LastSeenAt:     now,
		},
		{
			Provider:       "gcp",
			Account:        "consize-staging",
			Region:         "us-central1",
			ResourceType:   "compute_disk",
			ResourceID:     "disk-abandoned-ci-runner",
			Name:           "disk-abandoned-ci-runner",
			MonthlyCost:    45.50,
			Recommendation: "Delete unattached disk",
			Action:         "delete",
			Risk:           "low",
			Status:         "open",
			FirstSeenAt:    now.Add(-60 * 24 * time.Hour),
			LastSeenAt:     now,
		},
	}

	if err := st.UpsertCostOpportunities(ctx, opps); err != nil {
		log.Printf("seed waste error: %v", err)
	} else {
		fmt.Printf("seeded %d cloud waste opportunities\n", len(opps))
	}
}

func seedHistory(ctx context.Context, st store.Store) {
	// Fetch all recommendations
	recs, _, err := st.ListRecommendations(ctx, nil, "", 100, 0)
	if err != nil {
		log.Printf("seed history: list recs error: %v", err)
		return
	}

	// Find the recommendation for payments-service (or inventory-service if payments not found)
	var targetRec store.Recommendation
	found := false
	for _, r := range recs {
		// Just grab the first one that has substantial savings but isn't checkout-api
		if r.SavingsMonthly > 5.0 && r.Resource == "memory" {
			wl, _ := st.GetWorkload(ctx, r.WorkloadID)
			if wl.Name == "payments-service" {
				targetRec = r
				found = true
				break
			}
		}
	}

	if !found {
		return
	}

	// Create an ApplyEvent for it in the past (e.g. 2 hours ago)
	applyTime := time.Now().UTC().Add(-2 * time.Hour)
	diff := store.Diff{
		Resource:        targetRec.Resource,
		CurrentReq:      targetRec.CurrentValue,
		ProposedReq:     targetRec.ProposedValue,
		CurrentLimit:    targetRec.CurrentLimit,
		ProposedLimit:   targetRec.ProposedLimit,
	}
	
	applyID, err := st.CreateApplyEvent(ctx, store.ApplyEvent{
		RecommendationID: targetRec.ID,
		WorkloadID:       targetRec.WorkloadID,
		Actor:            "Alex (DevOps)",
		Mode:             "approved",
		Result:           "applied",
		Diff:             diff,
		StepNumber:       1,
		TotalSteps:       2,
		CreatedAt:        applyTime,
	})
	if err != nil {
		log.Printf("seed history: create apply event error: %v", err)
		return
	}

	// Create a VerificationRun (Passed) 1 hour ago
	verifyTime := time.Now().UTC().Add(-1 * time.Hour)
	err = st.CreateVerificationRun(ctx, store.VerificationRun{
		ApplyEventID:  applyID,
		Verdict:       "passed",
		BaselineStart: applyTime.Add(-10 * time.Minute),
		BaselineEnd:   applyTime,
		PostStart:     applyTime,
		PostEnd:       verifyTime,
		SLIs: map[string]any{
			"oom_killed": map[string]any{"signal": "oom_killed", "verdict": "passed"},
			"restarts":   map[string]any{"signal": "restarts", "verdict": "passed"},
		},
		Thresholds: map[string]any{
			"window": "1h",
		},
		CreatedAt: verifyTime,
	})
	if err != nil {
		log.Printf("seed history: create verification run error: %v", err)
		return
	}

	// Update the recommendation status to verified
	_ = st.SetRecommendationStatus(ctx, targetRec.ID, store.StatusVerified)

	// Create a follow-up recommendation (Step 2)
	finalValue := int64(float64(targetRec.ProposedValue) * 0.8) // Some arbitrary further reduction
	finalLimit := int64(float64(targetRec.ProposedLimit) * 0.8)
	_, err = st.CreateFollowUpRecommendation(ctx, store.Recommendation{
		WorkloadID:     targetRec.WorkloadID,
		Resource:       targetRec.Resource,
		CurrentValue:   targetRec.ProposedValue,
		ProposedValue:  finalValue,
		CurrentLimit:   targetRec.ProposedLimit,
		ProposedLimit:  finalLimit,
		SavingsMonthly: 12.50,
		Confidence:     0.9,
		PolicyVersion:  targetRec.PolicyVersion,
		Status:         store.StatusPending,
		StepNumber:     2,
		TotalSteps:     2,
	})
	if err != nil {
		log.Printf("seed history: create follow up error: %v", err)
	}

	fmt.Printf("seeded historical apply event (ID: %d), verification run, and continuation for workload %d\n", applyID, targetRec.WorkloadID)
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
