// Package dbapply is the safety layer for database class changes
// (ADR-030/031). It mirrors the k8s apply engine's guardrail pipeline —
// store health, pending-only, exclusions win, mode policy, concurrency,
// audit trail — and adds the two DB-specific rules:
//
//   - maintenance window: a real apply must land inside the instance's
//     weekly maintenance window (UTC, "ddd:hh:mm-ddd:hh:mm"). Dry-runs
//     are exempt so operators can plan ahead, but report InWindow.
//     An empty or malformed window is fail-closed for every mode.
//   - one class step: a recommendation may span several catalog classes
//     (e.g. xlarge → micro); each apply moves exactly one adjacent step
//     and queues the remainder as a follow-up recommendation, so every
//     class change is a small, verifiable increment.
//
// The write surface is the ClassChanger seam — the cloud-provider
// implementation (CloudWatch/Cloud Monitoring) is deferred exactly like
// the k8s Patcher's live cluster, so the whole engine is testable
// against a recording fake.
package dbapply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"consize/internal/analysis"
	"consize/internal/store"
)

// Config tunes the DB guardrails.
type Config struct {
	// ConcurrentApplies is the global in-flight cap, shared with the k8s
	// surface via the store (applied, unverified events).
	ConcurrentApplies int
	// AutoDBLabel is the instance label whose value must be "enabled"
	// for mode=auto applies — the approval default (ADR-030): DB
	// changes never go automatic without an explicit opt-in.
	AutoDBLabel string
}

// DefaultConfig returns the shipped DB guardrail settings.
func DefaultConfig() Config {
	return Config{ConcurrentApplies: 1, AutoDBLabel: "consize.savings.dev/auto-db"}
}

// ClassChanger touches the DB provider. The only write surface Consize
// has for instances. Class changes are absolute writes (the target class
// names the full target state), so rollback needs no live read — the
// pre-apply class in the apply event is the rollback target (ADR-031).
type ClassChanger interface {
	// ChangeClass moves an instance to the given class.
	ChangeClass(ctx context.Context, wl store.Workload, class string) error
	// Health reports the provider's reachability (readiness probe).
	Health(ctx context.Context) error
}

// StubChanger is the placeholder provider until a live cloud integration
// lands (ADR-030 defers CloudWatch/Cloud Monitoring exactly like the k8s
// Patcher's live cluster was deferred). It fails every write with a
// clear message so a FAIL verdict escalates to manual intervention
// instead of silently doing nothing.
type StubChanger struct{}

func (StubChanger) ChangeClass(_ context.Context, wl store.Workload, class string) error {
	return fmt.Errorf("no DB provider configured — manual class change required for %s (target %s)", wl.Name, class)
}

func (StubChanger) Health(context.Context) error { return nil }

// GuardError is a blocked apply with the reasons the guardrails found.
type GuardError struct {
	Reasons []string
}

func (e *GuardError) Error() string {
	return fmt.Sprintf("db apply blocked: %v", e.Reasons)
}

// ErrNotFound wraps missing recommendations/workloads.
var ErrNotFound = errors.New("not found")

// Result is the outcome of one Apply call.
type Result struct {
	EventID      int64
	DryRun       bool
	Applied      bool
	Diff         store.Diff
	StepNumber   int
	TotalSteps   int
	FollowUpID   int64 // > 0 when a remainder step was queued
	Blocked      bool
	BlockReasons []string
	InWindow     bool   // maintenance-window state (dry-run report)
	Window       string // human-readable window, e.g. "mon 02:00–03:00 UTC"
}

// Service evaluates guardrails and applies class changes.
type Service struct {
	st      store.Store
	changer ClassChanger
	cfg     Config
	now     func() time.Time // injected for tests
	log     *slog.Logger
}

// NewService wires the DB apply engine.
func NewService(st store.Store, changer ClassChanger, cfg Config) *Service {
	return &Service{st: st, changer: changer, cfg: cfg, now: time.Now, log: slog.Default()}
}

// Health reports readiness: the store (audit trail must be writable,
// ADR-008) and the DB provider.
func (s *Service) Health(ctx context.Context) error {
	if err := s.st.Health(ctx); err != nil {
		return err
	}
	return s.changer.Health(ctx)
}

// Apply runs the guardrail pipeline and, for non-dry modes, changes the
// class.
//
//  1. store health          — fail-safe: never apply without the audit trail
//  2. recommendation state  — pending only
//  3. exclusions win        — exclude label, data-loss-risk, protected ns
//  4. mode policy           — auto needs the auto-db label; approved needs an actor
//  5. maintenance window    — real applies inside the weekly window only
//  6. one class step        — never more than one adjacent catalog step
//  7. concurrency           — one in-flight apply per namespace, global cap
//
// Every evaluation is recorded as an apply_event (planned or applied).
func (s *Service) Apply(ctx context.Context, recID int64, mode, actor string) (Result, error) {
	var res Result

	// 1. Fail-safe: the audit trail must be writable (ADR-008).
	if err := s.st.Health(ctx); err != nil {
		return res, fmt.Errorf("store unhealthy — applies blocked (fail-safe): %w", err)
	}

	// 2. Recommendation must exist and be pending.
	rec, err := s.st.GetRecommendation(ctx, recID)
	if err != nil {
		return res, ErrNotFound
	}
	if rec.Status != store.StatusPending {
		return res, &GuardError{Reasons: []string{fmt.Sprintf("recommendation status is %q, not pending", rec.Status)}}
	}
	if rec.Resource != store.ResourceClass {
		return res, &GuardError{Reasons: []string{fmt.Sprintf("recommendation resource is %q, not class", rec.Resource)}}
	}

	// 3. Exclusions win.
	wl, err := s.st.GetWorkload(ctx, rec.WorkloadID)
	if err != nil {
		return res, ErrNotFound
	}
	if blocked := guardExclusions(wl); len(blocked) > 0 {
		return res, &GuardError{Reasons: blocked}
	}

	// 4. Mode policy.
	switch mode {
	case "dry_run":
		res.DryRun = true
	case "approved":
		if actor == "" {
			return res, &GuardError{Reasons: []string{"mode=approved requires an actor"}}
		}
	case "auto":
		if wl.Labels[s.cfg.AutoDBLabel] != "enabled" {
			return res, &GuardError{Reasons: []string{
				fmt.Sprintf("instance %s is not labeled %s=enabled (approval default: DB changes are never automatic without opt-in)", wl.Name, s.cfg.AutoDBLabel)}}
		}
	default:
		return res, &GuardError{Reasons: []string{"mode must be dry_run | approved | auto"}}
	}

	// 5. Maintenance window. Fail-closed when unconfigured or malformed;
	// dry-runs are exempt from the timing guard (planning ahead is the
	// point of a dry-run) but still report the window state.
	start, end, err := parseWindow(wl.DBMaintenanceWindow)
	res.Window = wl.DBMaintenanceWindow
	if err != nil {
		return res, &GuardError{Reasons: []string{"maintenance window: " + err.Error()}}
	}
	res.InWindow = inWindow(start, end, s.now())
	if !res.DryRun && !res.InWindow {
		return res, &GuardError{Reasons: []string{
			fmt.Sprintf("outside maintenance window (%s UTC) — instance changes land in the window only", wl.DBMaintenanceWindow)}}
	}

	// 6. One class step: the applied class is the adjacent catalog step
	// toward the target; the remainder becomes a follow-up.
	diff, total, followUp, err := s.stepPlan(rec)
	if err != nil {
		return res, err
	}
	res.Diff = diff
	res.StepNumber = 1
	res.TotalSteps = total

	// 7. Concurrency.
	if active, err := s.st.ActiveApplyInNamespace(ctx, wl.Namespace); err != nil {
		return res, err
	} else if active {
		return res, &GuardError{Reasons: []string{
			fmt.Sprintf("namespace %s has an in-flight apply (verify before applying again)", wl.Namespace)}}
	}
	if n, err := s.st.ActiveApplyCount(ctx); err != nil {
		return res, err
	} else if n >= s.cfg.ConcurrentApplies {
		return res, &GuardError{Reasons: []string{
			fmt.Sprintf("global concurrency limit reached (%d in-flight)", n)}}
	}

	if res.DryRun {
		// Record the evaluation; touch nothing.
		id, err := s.st.CreateApplyEvent(ctx, store.ApplyEvent{
			RecommendationID: rec.ID, WorkloadID: wl.ID,
			Actor: actorOr(actor, "dry_run"), Mode: "dry_run",
			Result: store.EventPlanned, Diff: diff,
			StepNumber: res.StepNumber, TotalSteps: res.TotalSteps,
		})
		if err != nil {
			return res, err
		}
		res.EventID = id
		res.Blocked = false
		return res, nil
	}

	// Real apply: change class, then record (audit-first storage health
	// already guaranteed the trail is writable).
	if err := s.changer.ChangeClass(ctx, wl, diff.ClassProposed); err != nil {
		return res, fmt.Errorf("change class %s/%s: %w", wl.Namespace, wl.Name, err)
	}
	id, err := s.st.CreateApplyEvent(ctx, store.ApplyEvent{
		RecommendationID: rec.ID, WorkloadID: wl.ID,
		Actor: actor, Mode: mode,
		Result: store.EventApplied, Diff: diff,
		StepNumber: res.StepNumber, TotalSteps: res.TotalSteps,
	})
	if err != nil {
		return res, err
	}
	if err := s.st.SetRecommendationStatus(ctx, rec.ID, store.StatusApplied); err != nil {
		return res, err
	}
	res.EventID = id
	res.Applied = true

	// Queue the remainder as a follow-up pending recommendation.
	if followUp != nil {
		fid, err := s.st.CreateFollowUpRecommendation(ctx, *followUp)
		if err != nil {
			return res, err
		}
		res.FollowUpID = fid
	}
	return res, nil
}

// Rollback restores an instance to an applied event's pre-apply class
// and records the inverse as a reverted event. The verifier calls this
// on a FAIL verdict. The restore is the absolute pre-apply class from
// the event — never a delta — so it lands correctly even if the live
// class drifted during the window (same rationale as ADR-026).
func (s *Service) Rollback(ctx context.Context, event store.ApplyEvent) error {
	wl, err := s.st.GetWorkload(ctx, event.WorkloadID)
	if err != nil {
		return err
	}
	if err := s.changer.ChangeClass(ctx, wl, event.Diff.ClassCurrent); err != nil {
		return fmt.Errorf("rollback class %s/%s: %w", wl.Namespace, wl.Name, err)
	}
	_, err = s.st.CreateApplyEvent(ctx, store.ApplyEvent{
		RecommendationID: event.RecommendationID, WorkloadID: event.WorkloadID,
		Actor: "verifier", Mode: event.Mode,
		Result: store.EventReverted, Diff: store.Diff{
			Resource:      store.ResourceClass,
			ClassCurrent:  event.Diff.ClassProposed,
			ClassProposed: event.Diff.ClassCurrent,
		},
		StepNumber: event.StepNumber, TotalSteps: event.TotalSteps,
	})
	if err != nil {
		return err
	}
	return s.st.SetRecommendationStatus(ctx, event.RecommendationID, store.StatusRolled)
}

// guardExclusions returns the exclusion reasons (empty = eligible).
// Mirrors the k8s apply engine's rules: a database is just as protected.
func guardExclusions(wl store.Workload) []string {
	var out []string
	if wl.Labels["consize.savings.dev/exclude"] == "true" {
		out = append(out, "excluded by label consize.savings.dev/exclude=true")
	}
	if wl.Labels["consize.savings.dev/data-loss-risk"] == "true" {
		out = append(out, "stateful workload flagged data-loss-risk")
	}
	switch wl.Namespace {
	case "kube-system", "consize-system":
		out = append(out, "protected namespace "+wl.Namespace)
	}
	return out
}

// stepPlan computes this apply's class (one adjacent catalog step toward
// the target) and the follow-up recommendation that continues when the
// target is farther. Downsize-only, like the k8s policy.
func (s *Service) stepPlan(rec store.Recommendation) (store.Diff, int, *store.Recommendation, error) {
	curIdx := classIndex(rec.ClassCurrent)
	if curIdx < 0 {
		return store.Diff{}, 0, nil, errors.New("unknown current class " + rec.ClassCurrent)
	}
	targetIdx := classIndex(rec.ClassProposed)
	if targetIdx < 0 {
		return store.Diff{}, 0, nil, errors.New("unknown proposed class " + rec.ClassProposed)
	}
	if targetIdx >= curIdx {
		return store.Diff{}, 0, nil, errors.New("nothing to reduce (downsize-only)")
	}

	adjacent, ok := analysis.DBClassStep(rec.ClassCurrent) // one step toward the target
	if !ok {
		// Unknown or already-cheapest class: unreachable for healthy
		// recommendations (dbSizing never proposes a class with no
		// cheaper step), but fail closed.
		return store.Diff{}, 0, nil, errors.New("no cheaper class step from " + rec.ClassCurrent)
	}
	diff := store.Diff{
		Resource:      store.ResourceClass,
		ClassCurrent:  rec.ClassCurrent,
		ClassProposed: adjacent.Name,
	}

	var followUp *store.Recommendation
	if adjacent.Name != rec.ClassProposed {
		next := rec
		next.ID = 0
		next.Status = store.StatusPending
		next.ClassCurrent = adjacent.Name
		// The remainder's savings are the price difference it will close.
		next.SavingsMonthly = adjacent.PriceUSD - classPrice(rec.ClassProposed)
		followUp = &next
	}
	return diff, curIdx - targetIdx, followUp, nil
}

// classIndex returns the catalog index of a class, or -1.
func classIndex(name string) int {
	return analysis.DBClassIndex(name)
}

// InMaintenanceWindow reports whether now falls inside a weekly
// maintenance window string ("ddd:hh:mm-ddd:hh:mm", UTC) — exported for
// the API's recommendation risk flags. Empty or malformed windows error
// (fail-closed, mirroring the apply path).
func InMaintenanceWindow(win string, now time.Time) (bool, error) {
	start, end, err := parseWindow(win)
	if err != nil {
		return false, err
	}
	return inWindow(start, end, now), nil
}

func classPrice(name string) float64 {
	if c, ok := analysis.DBClassByName(name); ok {
		return c.PriceUSD
	}
	return 0
}

func actorOr(actor, def string) string {
	if actor == "" {
		return def
	}
	return actor
}

// Maintenance-window parsing (UTC, weekly-recurring). Format:
// "ddd:hh:mm-ddd:hh:mm", e.g. "mon:02:00-mon:03:00". The interval is
// start-inclusive, end-exclusive; end < start wraps past midnight
// (e.g. "sun:23:00-mon:01:00"). end == start is invalid — an
// unparseable or unconfigured window is fail-closed.

var dayIndex = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

func parseWindow(s string) (start, end int, err error) {
	if s == "" {
		return 0, 0, errors.New("no maintenance window configured")
	}
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("malformed window %q (want ddd:hh:mm-ddd:hh:mm)", s)
	}
	start, err = parseDayTime(parts[0])
	if err != nil {
		return 0, 0, err
	}
	end, err = parseDayTime(parts[1])
	if err != nil {
		return 0, 0, err
	}
	if end == start {
		return 0, 0, fmt.Errorf("malformed window %q (end equals start)", s)
	}
	return start, end, nil
}

// parseDayTime parses "ddd:hh:mm" into minutes since Sunday 00:00 UTC.
func parseDayTime(s string) (int, error) {
	day, rest, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("malformed day-time %q (want ddd:hh:mm)", s)
	}
	dow, ok := dayIndex[strings.ToLower(day)]
	if !ok {
		return 0, fmt.Errorf("unknown weekday %q", day)
	}
	var h, m int
	if _, err := fmt.Sscanf(rest, "%d:%d", &h, &m); err != nil {
		return 0, fmt.Errorf("malformed time %q (want hh:mm)", rest)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("time out of range %q", rest)
	}
	return dow*1440 + h*60 + m, nil
}

// minuteOfWeek converts a time to minutes since Sunday 00:00 UTC.
func minuteOfWeek(t time.Time) int {
	t = t.UTC()
	return int(t.Weekday())*1440 + t.Hour()*60 + t.Minute()
}

// inWindow reports whether now falls inside the weekly window. Windows
// that wrap past midnight (end < start) span two intervals.
func inWindow(start, end int, now time.Time) bool {
	m := minuteOfWeek(now)
	if start < end {
		return m >= start && m < end
	}
	return m >= start || m < end
}
