package dbapply

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"consize/internal/store"
)

// fakeChanger records every class change it is asked to make — the
// recording-fake equivalent of the k8s patcher fakes.
type fakeChanger struct {
	mu      sync.Mutex
	class   string // live class
	changes []string
	health  error
}

func (f *fakeChanger) ChangeClass(_ context.Context, _ store.Workload, class string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.class = class
	f.changes = append(f.changes, class)
	return nil
}

func (f *fakeChanger) Health(_ context.Context) error { return f.health }

func (f *fakeChanger) live() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.class
}

// nextWeekday returns the first time with the given weekday/h:m on or
// after the Unix epoch (Thursday 1970-01-01), so every test time is
// deterministic without calendar assumptions.
func nextWeekday(dow time.Weekday, h, m int) time.Time {
	t := time.Unix(0, 0).UTC()
	for t.Weekday() != dow {
		t = t.Add(24 * time.Hour)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), h, m, 0, 0, time.UTC)
}

// seed rec seeds a pending class recommendation and returns its ID.
func seedRec(t *testing.T, st store.Store, wl store.Workload, cur, prop string, savings float64) (int64, int64) {
	t.Helper()
	wid, err := st.UpsertWorkload(ctx, wl)
	if err != nil {
		t.Fatal(err)
	}
	rec := store.Recommendation{
		WorkloadID:     wid,
		Resource:       store.ResourceClass,
		ClassCurrent:   cur,
		ClassProposed:  prop,
		SavingsMonthly: savings,
		Confidence:     1.0,
		PolicyVersion:  "test",
		Status:         store.StatusPending,
	}
	if err := st.CreateRecommendations(ctx, []store.Recommendation{rec}); err != nil {
		t.Fatal(err)
	}
	recs, _, err := st.ListRecommendations(ctx, &wid, store.StatusPending, 0, 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("seed: n=%d err=%v", len(recs), err)
	}
	return wid, recs[0].ID
}

var ctx = context.Background()

func TestApplyApprovedInsideWindow(t *testing.T) {
	st := store.NewMemory()
	ch := &fakeChanger{class: "db.t3.large"}
	svc := &Service{st: st, changer: ch, cfg: DefaultConfig(),
		now: func() time.Time { return nextWeekday(time.Tuesday, 2, 30) }}

	_, recID := seedRec(t, st, store.Workload{
		Name: "payments-prod", Namespace: "db", Kind: "database",
		Source: "db", DBClass: "db.t3.large",
		DBMaintenanceWindow: "tue:02:00-tue:03:00",
	}, "db.t3.large", "db.t3.medium", 50)

	res, err := svc.Apply(ctx, recID, "approved", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.DryRun {
		t.Fatalf("want applied, got %+v", res)
	}
	if !res.InWindow {
		t.Fatal("window should be reported in-window")
	}
	if res.Diff.ClassCurrent != "db.t3.large" || res.Diff.ClassProposed != "db.t3.medium" {
		t.Fatalf("diff: %+v", res.Diff)
	}
	if res.StepNumber != 1 || res.TotalSteps != 1 || res.FollowUpID != 0 {
		t.Fatalf("single-step apply: %+v", res)
	}
	if got := ch.live(); got != "db.t3.medium" {
		t.Fatalf("live class: want db.t3.medium, got %s", got)
	}

	// Audit trail: applied event + recommendation status.
	events, err := st.ListApplyEvents(ctx, nil, store.EventApplied)
	if err != nil || len(events) != 1 {
		t.Fatalf("want 1 applied event, got %d err=%v", len(events), err)
	}
	if events[0].Actor != "alice" || events[0].Mode != "approved" {
		t.Fatalf("event evidence: %+v", events[0])
	}
	rec, err := st.GetRecommendation(ctx, recID)
	if err != nil || rec.Status != store.StatusApplied {
		t.Fatalf("rec status: want applied, got %v err=%v", rec.Status, err)
	}
}

func TestApplyOneClassStepQueuesFollowUp(t *testing.T) {
	st := store.NewMemory()
	ch := &fakeChanger{class: "db.t3.xlarge"}
	svc := &Service{st: st, changer: ch, cfg: DefaultConfig(),
		now: func() time.Time { return nextWeekday(time.Tuesday, 2, 30) }}

	_, recID := seedRec(t, st, store.Workload{
		Name: "big", Namespace: "db", Kind: "database",
		Source: "db", DBClass: "db.t3.xlarge",
		DBMaintenanceWindow: "tue:02:00-tue:03:00",
	}, "db.t3.xlarge", "db.t3.micro", 185)

	res, err := svc.Apply(ctx, recID, "approved", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if res.Diff.ClassProposed != "db.t3.large" {
		t.Fatalf("one class step: want adjacent db.t3.large, got %+v", res.Diff)
	}
	if res.TotalSteps != 4 || res.StepNumber != 1 {
		t.Fatalf("want 4 total steps (xlarge→large→medium→small→micro), got %+v", res)
	}
	if res.FollowUpID == 0 {
		t.Fatal("want a follow-up recommendation for the remainder")
	}
	if got := ch.live(); got != "db.t3.large" {
		t.Fatalf("live class: want db.t3.large (one step only), got %s", got)
	}

	fu, err := st.GetRecommendation(ctx, res.FollowUpID)
	if err != nil {
		t.Fatal(err)
	}
	if fu.ClassCurrent != "db.t3.large" || fu.ClassProposed != "db.t3.micro" {
		t.Fatalf("follow-up pair: %s → %s", fu.ClassCurrent, fu.ClassProposed)
	}
	if fu.Status != store.StatusPending {
		t.Fatalf("follow-up must be pending, got %s", fu.Status)
	}
	if fu.StepNumber != 2 || fu.TotalSteps != 4 {
		t.Fatalf("follow-up must continue original chain as step 2/4, got %d/%d", fu.StepNumber, fu.TotalSteps)
	}
	if fu.SavingsMonthly != 85 { // price(large) − price(micro)
		t.Fatalf("follow-up savings: want $85/mo, got %v", fu.SavingsMonthly)
	}
}

func TestApplyDryRunOutsideWindowPlansButDoesNotTouch(t *testing.T) {
	st := store.NewMemory()
	ch := &fakeChanger{class: "db.t3.large"}
	svc := &Service{st: st, changer: ch, cfg: DefaultConfig(),
		now: func() time.Time { return nextWeekday(time.Monday, 10, 0) }} // window is Tue 02:00–03:00

	_, recID := seedRec(t, st, store.Workload{
		Name: "payments-prod", Namespace: "db", Kind: "database",
		Source: "db", DBClass: "db.t3.large",
		DBMaintenanceWindow: "tue:02:00-tue:03:00",
	}, "db.t3.large", "db.t3.medium", 50)

	// Dry-run: planned, nothing touched, window state reported.
	res, err := svc.Apply(ctx, recID, "dry_run", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || res.InWindow {
		t.Fatalf("want dry-run outside window reported, got %+v", res)
	}
	if ch.live() != "db.t3.large" {
		t.Fatal("dry-run must not change the class")
	}
	events, err := st.ListApplyEvents(ctx, nil, store.EventPlanned)
	if err != nil || len(events) != 1 {
		t.Fatalf("want 1 planned event, got %d err=%v", len(events), err)
	}

	// Real apply outside the window: blocked.
	_, err = svc.Apply(ctx, recID, "approved", "alice")
	var ge *GuardError
	if !errors.As(err, &ge) {
		t.Fatalf("want GuardError outside window, got %v", err)
	}
	if len(ge.Reasons) == 0 || ge.Reasons[0] != "outside maintenance window (tue:02:00-tue:03:00 UTC) — instance changes land in the window only" {
		t.Fatalf("window reason: %v", ge.Reasons)
	}
}

func TestApplyFailClosedWithoutWindow(t *testing.T) {
	st := store.NewMemory()
	ch := &fakeChanger{class: "db.t3.large"}
	svc := &Service{st: st, changer: ch, cfg: DefaultConfig(),
		now: func() time.Time { return nextWeekday(time.Tuesday, 2, 30) }}

	_, recID := seedRec(t, st, store.Workload{
		Name: "nowindow", Namespace: "db", Kind: "database",
		Source: "db", DBClass: "db.t3.large",
	}, "db.t3.large", "db.t3.medium", 50)

	// An unconfigured window is fail-closed for every mode — including
	// dry-run: there is nothing to plan against.
	for _, mode := range []string{"dry_run", "approved"} {
		_, err := svc.Apply(ctx, recID, mode, "alice")
		var ge *GuardError
		if !errors.As(err, &ge) || len(ge.Reasons) == 0 || ge.Reasons[0] != "maintenance window: no maintenance window configured" {
			t.Fatalf("mode %s: want fail-closed no-window guard, got %v", mode, err)
		}
	}
}

func TestApplyWrapAroundWindow(t *testing.T) {
	start, end, err := parseWindow("sun:23:00-mon:01:00")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		when time.Time
		in   bool
	}{
		{nextWeekday(time.Sunday, 23, 30), true},  // inside the late-Sunday half
		{nextWeekday(time.Monday, 0, 30), true},   // inside the early-Monday half
		{nextWeekday(time.Monday, 2, 0), false},   // after the window
		{nextWeekday(time.Sunday, 22, 59), false}, // before the window
	}
	for _, c := range cases {
		if got := inWindow(start, end, c.when); got != c.in {
			t.Fatalf("inWindow(%s): want %v, got %v", c.when, c.in, got)
		}
	}
}

func TestApplyAutoRequiresAutoDBLabel(t *testing.T) {
	st := store.NewMemory()
	ch := &fakeChanger{class: "db.t3.large"}
	svc := &Service{st: st, changer: ch, cfg: DefaultConfig(),
		now: func() time.Time { return nextWeekday(time.Tuesday, 2, 30) }}

	wid, recID := seedRec(t, st, store.Workload{
		Name: "payments-prod", Namespace: "db", Kind: "database",
		Source: "db", DBClass: "db.t3.large",
		DBMaintenanceWindow: "tue:02:00-tue:03:00",
		Labels:              map[string]string{"consize.savings.dev/auto-db": "disabled"},
	}, "db.t3.large", "db.t3.medium", 50)

	_, err := svc.Apply(ctx, recID, "auto", "")
	var ge *GuardError
	if !errors.As(err, &ge) || len(ge.Reasons) != 1 {
		t.Fatalf("want approval-default guard, got %v", err)
	}

	// Opt in → auto applies. The blocked attempt left the recommendation
	// pending (a blocked apply records no event and no status change).
	wl, err := st.GetWorkload(ctx, wid)
	if err != nil {
		t.Fatal(err)
	}
	wl.Labels["consize.savings.dev/auto-db"] = "enabled"
	if _, err := st.UpsertWorkload(ctx, wl); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Apply(ctx, recID, "auto", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied {
		t.Fatalf("auto apply with opt-in label: %+v", res)
	}
}

func TestApplyGuardMatrix(t *testing.T) {
	newSvc := func() (*Service, *fakeChanger, int64) {
		st := store.NewMemory()
		ch := &fakeChanger{class: "db.t3.large"}
		svc := &Service{st: st, changer: ch, cfg: DefaultConfig(),
			now: func() time.Time { return nextWeekday(time.Tuesday, 2, 30) }}
		_, recID := seedRec(t, st, store.Workload{
			Name: "guard", Namespace: "db", Kind: "database",
			Source: "db", DBClass: "db.t3.large",
			DBMaintenanceWindow: "tue:02:00-tue:03:00",
		}, "db.t3.large", "db.t3.medium", 50)
		return svc, ch, recID
	}

	t.Run("non-pending", func(t *testing.T) {
		svc, ch, recID := newSvc()
		_ = svc.st.SetRecommendationStatus(ctx, recID, store.StatusRejected)
		_, err := svc.Apply(ctx, recID, "approved", "alice")
		var ge *GuardError
		if !errors.As(err, &ge) || ge.Reasons[0] != `recommendation status is "rejected", not pending` {
			t.Fatalf("got %v", err)
		}
		_ = ch
	})
	t.Run("wrong-resource", func(t *testing.T) {
		st := store.NewMemory()
		ch := &fakeChanger{class: "db.t3.large"}
		svc := &Service{st: st, changer: ch, cfg: DefaultConfig(),
			now: func() time.Time { return nextWeekday(time.Tuesday, 2, 30) }}
		wid, err := st.UpsertWorkload(ctx, store.Workload{
			Name: "api", Namespace: "prod", Kind: "deployment", Source: "k8s",
		})
		if err != nil {
			t.Fatal(err)
		}
		// A CPU recommendation is out of this service's lane.
		if err := st.CreateRecommendations(ctx, []store.Recommendation{{
			WorkloadID: wid, Resource: store.ResourceCPU,
			CurrentValue: 500, ProposedValue: 300,
			SavingsMonthly: 20, PolicyVersion: "test", Status: store.StatusPending,
		}}); err != nil {
			t.Fatal(err)
		}
		recs, _, err := st.ListRecommendations(ctx, &wid, store.StatusPending, 0, 0)
		if err != nil || len(recs) != 1 {
			t.Fatalf("seed: n=%d err=%v", len(recs), err)
		}
		_, err = svc.Apply(ctx, recs[0].ID, "approved", "alice")
		var ge *GuardError
		if !errors.As(err, &ge) || ge.Reasons[0] != "recommendation resource is \"cpu\", not class" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("excluded-label", func(t *testing.T) {
		st := store.NewMemory()
		ch := &fakeChanger{class: "db.t3.large"}
		svc := &Service{st: st, changer: ch, cfg: DefaultConfig(),
			now: func() time.Time { return nextWeekday(time.Tuesday, 2, 30) }}
		_, recID := seedRec(t, st, store.Workload{
			Name: "excl", Namespace: "db", Kind: "database",
			Source: "db", DBClass: "db.t3.large",
			DBMaintenanceWindow: "tue:02:00-tue:03:00",
			Labels:              map[string]string{"consize.savings.dev/exclude": "true"},
		}, "db.t3.large", "db.t3.medium", 50)
		_, err := svc.Apply(ctx, recID, "approved", "alice")
		var ge *GuardError
		if !errors.As(err, &ge) || ge.Reasons[0] != "excluded by label consize.savings.dev/exclude=true" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("protected-namespace", func(t *testing.T) {
		st := store.NewMemory()
		ch := &fakeChanger{class: "db.t3.large"}
		svc := &Service{st: st, changer: ch, cfg: DefaultConfig(),
			now: func() time.Time { return nextWeekday(time.Tuesday, 2, 30) }}
		_, recID := seedRec(t, st, store.Workload{
			Name: "sys", Namespace: "kube-system", Kind: "database",
			Source: "db", DBClass: "db.t3.large",
			DBMaintenanceWindow: "tue:02:00-tue:03:00",
		}, "db.t3.large", "db.t3.medium", 50)
		_, err := svc.Apply(ctx, recID, "approved", "alice")
		var ge *GuardError
		if !errors.As(err, &ge) || ge.Reasons[0] != "protected namespace kube-system" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("actor-required", func(t *testing.T) {
		svc, _, recID := newSvc()
		_, err := svc.Apply(ctx, recID, "approved", "")
		var ge *GuardError
		if !errors.As(err, &ge) || ge.Reasons[0] != "mode=approved requires an actor" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("bad-mode", func(t *testing.T) {
		svc, _, recID := newSvc()
		_, err := svc.Apply(ctx, recID, "teleport", "alice")
		var ge *GuardError
		if !errors.As(err, &ge) || ge.Reasons[0] != "mode must be dry_run | approved | auto" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("in-flight-namespace", func(t *testing.T) {
		st := store.NewMemory()
		ch := &fakeChanger{class: "db.t3.large"}
		svc := &Service{st: st, changer: ch, cfg: DefaultConfig(),
			now: func() time.Time { return nextWeekday(time.Tuesday, 2, 30) }}
		_, recID := seedRec(t, st, store.Workload{
			Name: "a", Namespace: "db", Kind: "database",
			Source: "db", DBClass: "db.t3.large",
			DBMaintenanceWindow: "tue:02:00-tue:03:00",
		}, "db.t3.large", "db.t3.medium", 50)
		if _, err := svc.Apply(ctx, recID, "approved", "alice"); err != nil {
			t.Fatal(err)
		}
		// Second recommendation, same namespace, still in-flight.
		_, recID2 := seedRec(t, st, store.Workload{
			Name: "b", Namespace: "db", Kind: "database",
			Source: "db", DBClass: "db.t3.large",
			DBMaintenanceWindow: "tue:02:00-tue:03:00",
		}, "db.t3.large", "db.t3.medium", 50)
		_, err := svc.Apply(ctx, recID2, "approved", "alice")
		var ge *GuardError
		if !errors.As(err, &ge) || len(ge.Reasons) != 1 {
			t.Fatalf("want in-flight guard, got %v", err)
		}
	})
	t.Run("unknown-class", func(t *testing.T) {
		st := store.NewMemory()
		ch := &fakeChanger{class: "db.t3.large"}
		svc := &Service{st: st, changer: ch, cfg: DefaultConfig(),
			now: func() time.Time { return nextWeekday(time.Tuesday, 2, 30) }}
		_, recID := seedRec(t, st, store.Workload{
			Name: "ghost", Namespace: "db", Kind: "database",
			Source: "db", DBClass: "db.t3.large",
			DBMaintenanceWindow: "tue:02:00-tue:03:00",
		}, "db.t3.large", "db.generic.custom", 1)
		_, err := svc.Apply(ctx, recID, "approved", "alice")
		if err == nil {
			t.Fatal("want unknown-class error")
		}
	})
}

func TestRollbackRestoresAbsoluteClass(t *testing.T) {
	st := store.NewMemory()
	ch := &fakeChanger{class: "db.t3.large"}
	svc := &Service{st: st, changer: ch, cfg: DefaultConfig(),
		now: func() time.Time { return nextWeekday(time.Tuesday, 2, 30) }}

	wid, recID := seedRec(t, st, store.Workload{
		Name: "payments-prod", Namespace: "db", Kind: "database",
		Source: "db", DBClass: "db.t3.large",
		DBMaintenanceWindow: "tue:02:00-tue:03:00",
	}, "db.t3.large", "db.t3.medium", 50)

	res, err := svc.Apply(ctx, recID, "approved", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if res.EventID == 0 || !res.Applied {
		t.Fatalf("setup: %+v", res)
	}
	event, err := st.GetApplyEvent(ctx, res.EventID)
	if err != nil {
		t.Fatal(err)
	}

	// Drift the live class during the window (the regression a verifier
	// would catch) — rollback must still restore the pre-apply class.
	ch.class = "db.t3.small"

	if err := svc.Rollback(ctx, event); err != nil {
		t.Fatal(err)
	}
	if got := ch.live(); got != "db.t3.large" {
		t.Fatalf("rollback: want absolute restore to db.t3.large, got %s", got)
	}
	events, err := st.ListApplyEvents(ctx, &wid, store.EventReverted)
	if err != nil || len(events) != 1 {
		t.Fatalf("want 1 reverted event, got %d err=%v", len(events), err)
	}
	if events[0].Diff.ClassCurrent != "db.t3.medium" || events[0].Diff.ClassProposed != "db.t3.large" {
		t.Fatalf("reverted diff should invert the classes: %+v", events[0].Diff)
	}
	rec, err := st.GetRecommendation(ctx, recID)
	if err != nil || rec.Status != store.StatusRolled {
		t.Fatalf("rec status: want rolled_back, got %v err=%v", rec.Status, err)
	}
}
