package store_test

import (
	"context"
	"testing"
	"time"

	"consize/internal/store"
)

func TestApplyEventLifecycle(t *testing.T) {
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			wlID := upsert(t, st, ctx, "api", "prod")
			recID := createRec(t, st, ctx, wlID, 4000, 8000, 1200, 4000)

			eid, err := st.CreateApplyEvent(ctx, store.ApplyEvent{
				RecommendationID: recID, WorkloadID: wlID,
				Actor: "alice", Mode: "approved", Result: store.EventPlanned,
				Diff: store.Diff{Resource: "cpu", CurrentReq: 4000, ProposedReq: 2800,
					CurrentLimit: 8000, ProposedLimit: 6400},
				StepNumber: 1, TotalSteps: 2,
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := st.GetApplyEvent(ctx, eid)
			if err != nil {
				t.Fatal(err)
			}
			if got.Result != store.EventPlanned || got.Actor != "alice" {
				t.Fatalf("event round-trip: %+v", got)
			}
			if got.Diff.ProposedReq != 2800 || got.Diff.ProposedLimit != 6400 {
				t.Fatalf("diff JSONB round-trip: %+v", got.Diff)
			}

			events, err := st.ListApplyEvents(ctx, &wlID, "")
			if err != nil || len(events) != 1 {
				t.Fatalf("list by workload: %d events, err %v", len(events), err)
			}
			// Only a "planned" event exists — filtering by "applied"
			// must return nothing.
			applied, err := st.ListApplyEvents(ctx, nil, store.EventApplied)
			if err != nil {
				t.Fatalf("list by result: %v", err)
			} else if len(applied) != 0 {
				t.Fatalf("result filter leaked: %d", len(applied))
			}
		})
	}
}

func TestInFlightStateIsDerived(t *testing.T) {
	// ADR-008: in-flight = applied event with no verification run.
	// Derived, never stored — a crash mid-verify leaves a retryable state.
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			wlID := upsert(t, st, ctx, "api", "prod")
			recID := createRec(t, st, ctx, wlID, 4000, 8000, 2800, 6400)
			eid, err := st.CreateApplyEvent(ctx, store.ApplyEvent{
				RecommendationID: recID, WorkloadID: wlID,
				Actor: "verifier", Mode: "approved", Result: store.EventApplied,
				Diff: store.Diff{Resource: "cpu", CurrentReq: 4000, ProposedReq: 2800},
			})
			if err != nil {
				t.Fatal(err)
			}

			if active, err := st.ActiveApplyInNamespace(ctx, "prod"); err != nil || !active {
				t.Fatalf("in-flight apply must be active: %v, %v", active, err)
			}
			if active, _ := st.ActiveApplyInNamespace(ctx, "other"); active {
				t.Fatal("namespace isolation broken")
			}
			if n, err := st.ActiveApplyCount(ctx); err != nil || n != 1 {
				t.Fatalf("global count: %d, %v", n, err)
			}
			unv, err := st.AppliedEventsUnverified(ctx)
			if err != nil || len(unv) != 1 {
				t.Fatalf("unverified: %d, %v", len(unv), err)
			}

			// A verification run closes the window: nothing active remains.
			if err := st.CreateVerificationRun(ctx, store.VerificationRun{
				ApplyEventID: eid, Verdict: store.VerdictPassed,
				BaselineStart: time.Now().Add(-48 * time.Hour), BaselineEnd: time.Now().Add(-24 * time.Hour),
				PostStart: time.Now().Add(-24 * time.Hour), PostEnd: time.Now(),
				SLIs: map[string]any{"throttling": map[string]any{"verdict": "passed"}},
			}); err != nil {
				t.Fatal(err)
			}
			if active, _ := st.ActiveApplyInNamespace(ctx, "prod"); active {
				t.Fatal("verification must close the in-flight window")
			}
			if n, _ := st.ActiveApplyCount(ctx); n != 0 {
				t.Fatalf("global count after verify: %d", n)
			}
			if unv, _ := st.AppliedEventsUnverified(ctx); len(unv) != 0 {
				t.Fatalf("unverified after verify: %d", len(unv))
			}
		})
	}
}

func TestVerificationRunUpsertPerApplyEvent(t *testing.T) {
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			wlID := upsert(t, st, ctx, "api", "prod")
			recID := createRec(t, st, ctx, wlID, 4000, 8000, 2800, 6400)
			eid, _ := st.CreateApplyEvent(ctx, store.ApplyEvent{
				RecommendationID: recID, WorkloadID: wlID,
				Actor: "verifier", Mode: "approved", Result: store.EventApplied,
				Diff: store.Diff{Resource: "cpu", CurrentReq: 4000, ProposedReq: 2800},
			})
			// First verdict inconclusive, then the CronJob re-verifies
			// and overwrites — one row per apply event, latest wins.
			for _, verdict := range []string{store.VerdictInconclusive, store.VerdictPassed} {
				if err := st.CreateVerificationRun(ctx, store.VerificationRun{
					ApplyEventID: eid, Verdict: verdict,
					BaselineStart: time.Now().Add(-48 * time.Hour), BaselineEnd: time.Now().Add(-24 * time.Hour),
					PostStart: time.Now().Add(-24 * time.Hour), PostEnd: time.Now(),
				}); err != nil {
					t.Fatal(err)
				}
			}
			runs, err := st.ListVerificationRuns(ctx, &eid)
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 1 {
				t.Fatalf("expected exactly one run per apply event, got %d", len(runs))
			}
			if runs[0].Verdict != store.VerdictPassed {
				t.Fatalf("latest verdict must win: %q", runs[0].Verdict)
			}
		})
	}
}

func TestFollowUpDoesNotSupersedeExisting(t *testing.T) {
	// The follow-up represents an in-flight step plan, not fresh analysis
	// output — inserting it must NOT mark older pending recs superseded
	// (that would delete the very state the step plan continues).
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			wlID := upsert(t, st, ctx, "api", "prod")
			recID := createRec(t, st, ctx, wlID, 4000, 8000, 2800, 6400)

			fid, err := st.CreateFollowUpRecommendation(ctx, store.Recommendation{
				WorkloadID: wlID, Resource: "cpu",
				CurrentValue: 2800, ProposedValue: 2000,
				CurrentLimit: 6400, ProposedLimit: 4000,
				SavingsMonthly: 12.5, Confidence: 0.9, PolicyVersion: "v1",
				Status: store.StatusPending,
			})
			if err != nil {
				t.Fatal(err)
			}
			original, err := st.GetRecommendation(ctx, recID)
			if err != nil {
				t.Fatal(err)
			}
			if original.Status != store.StatusPending {
				t.Fatalf("follow-up must not supersede the original: %q", original.Status)
			}
			followUp, err := st.GetRecommendation(ctx, fid)
			if err != nil {
				t.Fatal(err)
			}
			if followUp.CurrentValue != 2800 || followUp.ProposedValue != 2000 {
				t.Fatalf("follow-up carries the step state: %+v", followUp)
			}
		})
	}
}

func upsert(t *testing.T, st store.Store, ctx context.Context, name, ns string) int64 {
	t.Helper()
	id, err := st.UpsertWorkload(ctx, store.Workload{Name: name, Namespace: ns, Kind: "deployment", Source: "k8s"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func createRec(t *testing.T, st store.Store, ctx context.Context, wlID int64,
	cur, curLim, prop, propLim int64) int64 {
	t.Helper()
	if err := st.CreateRecommendations(ctx, []store.Recommendation{{
		WorkloadID: wlID, Resource: "cpu",
		CurrentValue: cur, ProposedValue: prop,
		CurrentLimit: curLim, ProposedLimit: propLim,
		SavingsMonthly: 10, Confidence: 0.9, PolicyVersion: "v1",
	}}); err != nil {
		t.Fatal(err)
	}
	recs, _, err := st.ListRecommendations(ctx, &wlID, store.StatusPending, 0, 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("seed rec: %d recs, %v", len(recs), err)
	}
	return recs[0].ID
}
