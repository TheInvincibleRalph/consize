package report

import (
	"bytes"
	"context"
	"testing"
	"time"

	"consize/internal/store"
)

func TestNormalizeConfig(t *testing.T) {
	if cfg, err := NormalizeConfig(Config{}); err != nil || cfg.RangeDays != 7 {
		t.Fatalf("empty config should default to 7 days, got %+v err=%v", cfg, err)
	}
	for _, days := range []int{7, 14, 30} {
		if cfg, err := NormalizeConfig(Config{Enabled: true, RangeDays: days}); err != nil || cfg.RangeDays != days || !cfg.Enabled {
			t.Fatalf("valid days %d: %+v err=%v", days, cfg, err)
		}
	}
	if _, err := NormalizeConfig(Config{RangeDays: 90}); err == nil {
		t.Fatal("90-day reports are not part of the MVP contract")
	}
}

func TestBuildSummary(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	apiID, err := st.UpsertWorkload(ctx, store.Workload{Name: "api", Namespace: "prod", Source: "k8s"})
	if err != nil {
		t.Fatal(err)
	}
	jobsID, err := st.UpsertWorkload(ctx, store.Workload{Name: "jobs", Namespace: "prod", Source: "k8s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBucket(ctx, store.Bucket{
		WorkloadID: apiID, Metric: store.MetricCPUMilli, WindowStart: time.Now().UTC(),
		P50: 50, P95: 50, P99: 50, Max: 50, Samples: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRecommendations(ctx, []store.Recommendation{
		{WorkloadID: apiID, Resource: store.ResourceCPU, CurrentValue: 1000, ProposedValue: 700, SavingsMonthly: 10, Status: store.StatusPending},
		{WorkloadID: jobsID, Resource: store.ResourceMemory, CurrentValue: 1024 * 1024 * 1024, ProposedValue: 512 * 1024 * 1024, SavingsMonthly: 20, Status: store.StatusVerified},
	}); err != nil {
		t.Fatal(err)
	}
	recs, _, err := st.ListRecommendations(ctx, nil, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var verifiedRec store.Recommendation
	for _, rec := range recs {
		if rec.Status == store.StatusVerified {
			verifiedRec = rec
		}
	}
	applyID, err := st.CreateApplyEvent(ctx, store.ApplyEvent{
		RecommendationID: verifiedRec.ID,
		WorkloadID:       verifiedRec.WorkloadID,
		Actor:            "api:admin@example.com",
		Mode:             "approved",
		Result:           store.EventApplied,
		Diff: store.Diff{
			Resource:    store.ResourceMemory,
			CurrentReq:  1024 * 1024 * 1024,
			ProposedReq: 512 * 1024 * 1024,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateVerificationRun(ctx, store.VerificationRun{
		ApplyEventID: applyID,
		Verdict:      store.VerdictPassed,
		SLIs:         map[string]any{},
		Thresholds:   map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateApplyEvent(ctx, store.ApplyEvent{
		RecommendationID: verifiedRec.ID,
		WorkloadID:       verifiedRec.WorkloadID,
		Actor:            "api:admin@example.com",
		Mode:             "approved",
		Result:           store.EventReverted,
		Diff: store.Diff{
			Resource:    store.ResourceMemory,
			CurrentReq:  512 * 1024 * 1024,
			ProposedReq: 1024 * 1024 * 1024,
		},
	}); err != nil {
		t.Fatal(err)
	}

	summary, err := Build(ctx, st, time.Now().UTC().Add(time.Minute), 7)
	if err != nil {
		t.Fatal(err)
	}
	if summary.PendingRecommendations != 1 || summary.ProjectedMonthlySavings != 10 {
		t.Fatalf("pending summary mismatch: %+v", summary)
	}
	if summary.VerifiedApplies != 1 || summary.RealizedThisPeriodMonthlySavings != 20 {
		t.Fatalf("realized summary mismatch: %+v", summary)
	}
	if summary.Rollbacks != 1 {
		t.Fatalf("rollback count mismatch: %+v", summary)
	}
	if len(summary.TopPendingRecommendations) != 1 || summary.TopPendingRecommendations[0].WorkloadName != "api" {
		t.Fatalf("top pending mismatch: %+v", summary.TopPendingRecommendations)
	}
}

func TestPDF(t *testing.T) {
	pdf, err := PDF(Summary{
		GeneratedAt: time.Now().UTC(),
		From:        time.Now().UTC().AddDate(0, 0, -7),
		To:          time.Now().UTC(),
		RangeDays:   7,
		DataQuality: []string{"Telemetry is fresh enough for reporting."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-1.4")) ||
		!bytes.Contains(pdf, []byte("7-day savings report")) ||
		bytes.Contains(pdf, []byte("\xc3\xa2")) {
		t.Fatalf("unexpected pdf payload: %q", pdf[:min(len(pdf), 80)])
	}
}
