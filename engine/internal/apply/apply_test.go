package apply_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"consize/internal/apply"
	"consize/internal/store"
)

// fakePatcher records every patch and lets tests fail them or the
// cluster health probe. It also models the resource state a real
// deployment holds (single-container aggregate), so apply → drift →
// rollback sequences land on real totals — this is what proves the
// rollback targets the pre-apply values absolutely instead of onto
// drifted state.
type fakePatcher struct {
	mu        sync.Mutex
	patches   []patchCall
	healthErr error
	patchErr  error
	state     map[string]fakeResources // "ns/name" → aggregate req/lim
}

type fakeResources struct{ req, lim int64 }

type patchCall struct {
	namespace, name string
	diff            store.Diff
}

func (f *fakePatcher) PatchDeployment(_ context.Context, namespace, name string, d store.Diff) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patches = append(f.patches, patchCall{namespace, name, d})
	if f.patchErr != nil {
		return f.patchErr
	}
	if f.state == nil {
		f.state = map[string]fakeResources{}
	}
	cur := f.state[namespace+"/"+name]
	cur.req += d.ProposedReq - d.CurrentReq
	cur.lim += d.ProposedLimit - d.CurrentLimit
	f.state[namespace+"/"+name] = cur
	return nil
}

func (f *fakePatcher) ReadResources(_ context.Context, namespace, name, _ string) (req, lim int64, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur := f.state[namespace+"/"+name]
	return cur.req, cur.lim, nil
}

func (f *fakePatcher) Health(_ context.Context) error { return f.healthErr }

// setResources models an external actor changing the workload mid-window
// (the regression the verifier catches).
func (f *fakePatcher) setResources(namespace, name string, req, lim int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == nil {
		f.state = map[string]fakeResources{}
	}
	f.state[namespace+"/"+name] = fakeResources{req, lim}
}

func (f *fakePatcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.patches)
}

// brokenStore is a Store whose health probe always fails — the
// fail-safe must block applies while the audit trail is unreachable.
type brokenStore struct {
	store.Store
	err error
}

func (b brokenStore) Health(context.Context) error { return b.err }

func newService(t *testing.T, st store.Store, cfg apply.Config) (*apply.Service, *fakePatcher) {
	t.Helper()
	p := &fakePatcher{}
	return apply.NewService(st, p, cfg), p
}

// seedWorkload inserts a workload (with optional labels) and one pending
// recommendation, returning their IDs.
func seedWorkload(t *testing.T, st store.Store, name, ns string, labels map[string]string) int64 {
	t.Helper()
	id, err := st.UpsertWorkload(context.Background(), store.Workload{
		Name: name, Namespace: ns, Kind: "deployment", Source: "k8s", Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func seedRec(t *testing.T, st store.Store, wlID, cur, curLim, prop, propLim int64) int64 {
	t.Helper()
	if err := st.CreateRecommendations(context.Background(), []store.Recommendation{{
		WorkloadID: wlID, Resource: "cpu",
		CurrentValue: cur, ProposedValue: prop,
		CurrentLimit: curLim, ProposedLimit: propLim,
		SavingsMonthly: 50, Confidence: 0.9, PolicyVersion: "v1",
	}}); err != nil {
		t.Fatal(err)
	}
	recs, _, err := st.ListRecommendations(context.Background(), &wlID, store.StatusPending, 0, 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("seed rec: %d recs, err %v", len(recs), err)
	}
	return recs[0].ID
}

func expectBlocked(t *testing.T, res apply.Result, err error, wantReason string) {
	t.Helper()
	var ge *apply.GuardError
	if !errors.As(err, &ge) {
		t.Fatalf("expected GuardError, got %v (result %+v)", err, res)
	}
	found := false
	for _, r := range ge.Reasons {
		if r == wantReason {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing reason %q in %v", wantReason, ge.Reasons)
	}
	if res.Blocked {
		t.Fatal("blocked results must not set Blocked=true (guard errors, not outcomes)")
	}
}

func TestApplyBlockedByExclusions(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	svc, p := newService(t, st, apply.DefaultConfig())

	cases := []struct {
		name   string
		labels map[string]string
		ns     string
		reason string
	}{
		{"exclude label", map[string]string{"consize.savings.dev/exclude": "true"}, "prod",
			"excluded by label consize.savings.dev/exclude=true"},
		{"data loss risk", map[string]string{"consize.savings.dev/data-loss-risk": "true"}, "prod",
			"stateful workload flagged data-loss-risk"},
		{"protected namespace", nil, "kube-system", "protected namespace kube-system"},
		{"consize namespace", nil, "consize-system", "protected namespace consize-system"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wlID := seedWorkload(t, st, "api", tc.ns, tc.labels)
			recID := seedRec(t, st, wlID, 4000, 8000, 1200, 4000)
			res, err := svc.Apply(ctx, recID, "approved", "alice")
			expectBlocked(t, res, err, tc.reason)
			if p.count() != 0 {
				t.Fatal("no patch may reach the cluster from a blocked apply")
			}
		})
	}
}

func TestApplyBlockedNonPending(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	svc, p := newService(t, st, apply.DefaultConfig())

	wlID := seedWorkload(t, st, "api", "prod", nil)
	recID := seedRec(t, st, wlID, 4000, 8000, 1200, 4000)
	if err := st.SetRecommendationStatus(ctx, recID, store.StatusSuperseded); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Apply(ctx, recID, "approved", "alice")
	expectBlocked(t, res, err, `recommendation status is "superseded", not pending`)
	if p.count() != 0 {
		t.Fatal("no patch may reach the cluster from a blocked apply")
	}
}

func TestApplyModePolicy(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	svc, p := newService(t, st, apply.DefaultConfig())

	t.Run("approved requires actor", func(t *testing.T) {
		wlID := seedWorkload(t, st, "api", "prod", nil)
		recID := seedRec(t, st, wlID, 4000, 8000, 1200, 4000)
		res, err := svc.Apply(ctx, recID, "approved", "")
		expectBlocked(t, res, err, "mode=approved requires an actor")
	})
	t.Run("auto needs namespace label", func(t *testing.T) {
		wlID := seedWorkload(t, st, "api2", "prod", nil)
		recID := seedRec(t, st, wlID, 4000, 8000, 1200, 4000)
		res, err := svc.Apply(ctx, recID, "auto", "cicd")
		expectBlocked(t, res, err,
			"namespace prod is not labeled consize.savings.dev/auto-apply=enabled")
	})
	t.Run("auto with label applies", func(t *testing.T) {
		wlID := seedWorkload(t, st, "api3", "prod",
			map[string]string{"consize.savings.dev/auto-apply": "enabled"})
		recID := seedRec(t, st, wlID, 4000, 8000, 1200, 4000)
		res, err := svc.Apply(ctx, recID, "auto", "cicd")
		if err != nil {
			t.Fatal(err)
		}
		if !res.Applied || p.count() != 1 {
			t.Fatalf("auto apply failed: %+v", res)
		}
	})
	t.Run("unknown mode rejected", func(t *testing.T) {
		wlID := seedWorkload(t, st, "api4", "prod", nil)
		recID := seedRec(t, st, wlID, 4000, 8000, 1200, 4000)
		res, err := svc.Apply(ctx, recID, "sneaky", "alice")
		expectBlocked(t, res, err, "mode must be dry_run | approved | auto")
	})
}

func TestApplyDryRunTouchesNothing(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	svc, p := newService(t, st, apply.DefaultConfig())

	wlID := seedWorkload(t, st, "api", "prod", nil)
	recID := seedRec(t, st, wlID, 4000, 8000, 1200, 4000)

	res, err := svc.Apply(ctx, recID, "dry_run", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || res.Applied || p.count() != 0 {
		t.Fatalf("dry run must not patch: %+v", res)
	}
	events, err := st.ListApplyEvents(ctx, &wlID, store.EventPlanned)
	if err != nil || len(events) != 1 {
		t.Fatalf("expected one planned event, got %d (err %v)", len(events), err)
	}
	if events[0].Actor != "dry_run" {
		t.Fatalf("planned event actor: %q", events[0].Actor)
	}
	// The recommendation stays pending after a dry run.
	rec, err := st.GetRecommendation(ctx, recID)
	if err != nil || rec.Status != store.StatusPending {
		t.Fatalf("dry run must not consume the recommendation: %+v, %v", rec, err)
	}
}

func TestApplyApprovedPatchesAndAudits(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	svc, p := newService(t, st, apply.DefaultConfig())

	// 4000 → 3000 is a 25% reduction: inside the 30% step limit, so one
	// apply lands exactly on the target.
	wlID := seedWorkload(t, st, "api", "prod", nil)
	recID := seedRec(t, st, wlID, 4000, 8000, 3000, 5600)

	res, err := svc.Apply(ctx, recID, "approved", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.EventID == 0 {
		t.Fatalf("apply result: %+v", err)
	}
	if p.count() != 1 {
		t.Fatal("exactly one patch expected")
	}
	call := p.patches[0]
	if call.namespace != "prod" || call.name != "api" {
		t.Fatalf("patch target: %s/%s", call.namespace, call.name)
	}
	if call.diff.CurrentReq != 4000 || call.diff.ProposedReq != 3000 {
		t.Fatalf("diff for a within-step-limit rec must hit the target: %+v", call.diff)
	}
	events, _ := st.ListApplyEvents(ctx, &wlID, store.EventApplied)
	if len(events) != 1 {
		t.Fatalf("expected one applied event, got %d", len(events))
	}
	rec, _ := st.GetRecommendation(ctx, recID)
	if rec.Status != store.StatusApplied {
		t.Fatalf("status after apply: %q", rec.Status)
	}
}

func TestApplyStepSplittingQueuesFollowUp(t *testing.T) {
	// 4000 → 2000 with a 30% step limit needs two applies: 4000 → 2800,
	// then 2800 → 2000. The first apply queues a follow-up recommendation
	// that carries the remainder, savings scaled by reduction share.
	ctx := context.Background()
	st := store.NewMemory()
	svc, p := newService(t, st, apply.DefaultConfig())

	wlID := seedWorkload(t, st, "api", "prod", nil)
	recID := seedRec(t, st, wlID, 4000, 8000, 2000, 4000)

	res, err := svc.Apply(ctx, recID, "approved", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if res.StepNumber != 1 || res.TotalSteps != 2 {
		t.Fatalf("expected step 1 of 2, got %d of %d", res.StepNumber, res.TotalSteps)
	}
	call := p.patches[0]
	if call.diff.ProposedReq != 2800 || call.diff.ProposedLimit != 5600 {
		t.Fatalf("first step must stop at 30%%: req %d lim %d", call.diff.ProposedReq, call.diff.ProposedLimit)
	}
	if res.FollowUpID == 0 {
		t.Fatal("step split must queue a follow-up recommendation")
	}
	followUp, err := st.GetRecommendation(ctx, res.FollowUpID)
	if err != nil {
		t.Fatal(err)
	}
	if followUp.CurrentValue != 2800 || followUp.ProposedValue != 2000 {
		t.Fatalf("follow-up state: %+v", followUp)
	}
	if followUp.StepNumber != 2 || followUp.TotalSteps != 2 {
		t.Fatalf("follow-up must continue the original chain as step 2/2, got %d/%d", followUp.StepNumber, followUp.TotalSteps)
	}
	// Savings scale with the request share of the remaining reduction:
	// 2800 → 2000 is 800 of the 2000 total reduction = 40%.
	want := 50 * 800 / 2000.0
	if followUp.SavingsMonthly < want-0.01 || followUp.SavingsMonthly > want+0.01 {
		t.Fatalf("follow-up savings %.2f, want %.2f", followUp.SavingsMonthly, want)
	}

	// The in-flight guardrail blocks step 2 until step 1 verifies —
	// that is the safety engine's rhythm: apply, verify, apply.
	if _, err := svc.Apply(ctx, res.FollowUpID, "approved", "alice"); err == nil {
		t.Fatal("second step must be blocked while step 1 is unverified")
	}
	// Close the verification window with a passed run, as the verifier
	// CronJob would.
	if err := st.CreateVerificationRun(ctx, store.VerificationRun{
		ApplyEventID:  res.EventID,
		BaselineStart: time.Now().Add(-48 * time.Hour), BaselineEnd: time.Now().Add(-24 * time.Hour),
		PostStart: time.Now().Add(-24 * time.Hour), PostEnd: time.Now(),
		Verdict: store.VerdictPassed,
	}); err != nil {
		t.Fatal(err)
	}

	// The follow-up applies and lands on the target in one more step.
	res2, err := svc.Apply(ctx, res.FollowUpID, "approved", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if res2.Diff.ProposedReq != 2000 || res2.FollowUpID != 0 {
		t.Fatalf("second step must reach the target with no new follow-up: %+v", res2)
	}
	if res2.StepNumber != 2 || res2.TotalSteps != 2 {
		t.Fatalf("second apply must record step 2/2, got %d/%d", res2.StepNumber, res2.TotalSteps)
	}
}

func TestApplyBlockedByInFlightNamespace(t *testing.T) {
	// One in-flight apply per namespace: while an applied event has no
	// verification run, further applies in that namespace are blocked.
	ctx := context.Background()
	st := store.NewMemory()
	svc, _ := newService(t, st, apply.DefaultConfig())

	wlID := seedWorkload(t, st, "api", "prod", nil)
	recID := seedRec(t, st, wlID, 4000, 8000, 1200, 4000)
	if _, err := svc.Apply(ctx, recID, "approved", "alice"); err != nil {
		t.Fatal(err)
	}
	// A second recommendation in the same namespace must be blocked.
	recID2 := seedRec(t, st, wlID, 4000, 8000, 1000, 4000)
	res, err := svc.Apply(ctx, recID2, "approved", "alice")
	expectBlocked(t, res, err,
		"namespace prod has an in-flight apply (verify before applying again)")
}

func TestApplyBlockedByGlobalCap(t *testing.T) {
	// Global cap: with ConcurrentApplies=1, an in-flight apply anywhere
	// blocks an apply anywhere else, even in a clean namespace.
	ctx := context.Background()
	st := store.NewMemory()
	cfg := apply.DefaultConfig()
	cfg.ConcurrentApplies = 1
	svc, _ := newService(t, st, cfg)

	wlID := seedWorkload(t, st, "api", "prod", nil)
	recID := seedRec(t, st, wlID, 4000, 8000, 1200, 4000)
	if _, err := svc.Apply(ctx, recID, "approved", "alice"); err != nil {
		t.Fatal(err)
	}
	wlID2 := seedWorkload(t, st, "worker", "staging", nil)
	recID2 := seedRec(t, st, wlID2, 2000, 4000, 1000, 3000)
	res, err := svc.Apply(ctx, recID2, "approved", "bob")
	expectBlocked(t, res, err, "global concurrency limit reached (1 in-flight)")
}

func TestApplyBlockedByStoreFailure(t *testing.T) {
	// Fail-safe (ADR-008): applies never proceed when the audit trail is
	// unhealthy, even before looking at the recommendation.
	ctx := context.Background()
	st := store.NewMemory()
	down := errors.New("audit trail down")
	p := &fakePatcher{}
	svc := apply.NewService(brokenStore{Store: st, err: down}, p, apply.DefaultConfig())

	wlID := seedWorkload(t, st, "api", "prod", nil)
	recID := seedRec(t, st, wlID, 4000, 8000, 1200, 4000)
	_, err := svc.Apply(ctx, recID, "approved", "alice")
	if err == nil || !errors.Is(err, down) {
		t.Fatalf("fail-safe must block on unhealthy store, got %v", err)
	}
	if p.count() != 0 {
		t.Fatal("no patch may reach the cluster with an unhealthy store")
	}
}

func TestRollbackRestoresAndAudits(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	svc, p := newService(t, st, apply.DefaultConfig())

	wlID := seedWorkload(t, st, "api", "prod", nil)
	// The workload starts at the recommendation's current values.
	p.setResources("prod", "api", 4000, 8000)
	recID := seedRec(t, st, wlID, 4000, 8000, 2800, 6400)
	res, err := svc.Apply(ctx, recID, "approved", "alice")
	if err != nil || !res.Applied {
		t.Fatalf("apply: %v %+v", err, res)
	}
	if got, _, _ := p.ReadResources(ctx, "prod", "api", "cpu"); got != 2800 {
		t.Fatalf("apply must land on the proposed values, got %d", got)
	}
	appliedEvent, err := st.GetApplyEvent(ctx, res.EventID)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.Rollback(ctx, appliedEvent); err != nil {
		t.Fatal(err)
	}
	if len(p.patches) != 2 {
		t.Fatalf("expected apply + rollback patches, got %d", len(p.patches))
	}
	inv := p.patches[1].diff
	// The diff is expressed live → pre-apply (not a swap): the patcher
	// lands totals exactly on the pre-apply values.
	if inv.CurrentReq != 2800 || inv.ProposedReq != 4000 ||
		inv.CurrentLimit != 6400 || inv.ProposedLimit != 8000 {
		t.Fatalf("rollback must target pre-apply values: %+v", inv)
	}
	req, lim, _ := p.ReadResources(ctx, "prod", "api", "cpu")
	if req != 4000 || lim != 8000 {
		t.Fatalf("rollback must restore pre-apply state, got req=%d lim=%d", req, lim)
	}
	reverted, err := st.ListApplyEvents(ctx, &wlID, store.EventReverted)
	if err != nil || len(reverted) != 1 {
		t.Fatalf("expected one reverted event, got %d (err %v)", len(reverted), err)
	}
	if reverted[0].Actor != "verifier" {
		t.Fatalf("rollback actor: %q", reverted[0].Actor)
	}
	rec, _ := st.GetRecommendation(ctx, recID)
	if rec.Status != store.StatusRolled {
		t.Fatalf("recommendation status after rollback: %q", rec.Status)
	}
}

// TestRollbackAfterDrift is the live-cluster failure this fix exists
// for: the workload drifts during the verification window (a regression
// injected by a bad release — exactly what the verifier caught). A
// swapped-diff rollback lands on live+(preApply−applied), restoring
// nothing; the rollback must land on the recorded pre-apply values.
func TestRollbackAfterDrift(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	svc, p := newService(t, st, apply.DefaultConfig())

	wlID := seedWorkload(t, st, "api", "prod", nil)
	p.setResources("prod", "api", 4000, 8000)
	recID := seedRec(t, st, wlID, 4000, 8000, 2800, 6400)
	res, err := svc.Apply(ctx, recID, "approved", "alice")
	if err != nil || !res.Applied {
		t.Fatalf("apply: %v %+v", err, res)
	}
	appliedEvent, err := st.GetApplyEvent(ctx, res.EventID)
	if err != nil {
		t.Fatal(err)
	}

	// Regression injection mid-window: an external actor drops the
	// memory request far below the applied value.
	p.setResources("prod", "api", 1000, 2000)

	if err := svc.Rollback(ctx, appliedEvent); err != nil {
		t.Fatal(err)
	}
	inv := p.patches[1].diff
	// Expressed live → pre-apply: the delta the patcher distributes is
	// preApply − live, landing totals exactly on the pre-apply values.
	if inv.CurrentReq != 1000 || inv.ProposedReq != 4000 ||
		inv.CurrentLimit != 2000 || inv.ProposedLimit != 8000 {
		t.Fatalf("rollback diff must be live → pre-apply: %+v", inv)
	}
	req, lim, _ := p.ReadResources(ctx, "prod", "api", "cpu")
	if req != 4000 || lim != 8000 {
		t.Fatalf("drifted workload must be restored to pre-apply, got req=%d lim=%d", req, lim)
	}
}
