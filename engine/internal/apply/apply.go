// Package apply is the safety layer between recommendations and the
// cluster (docs/architecture.md §3.4). Every apply passes the guardrails
// — exclusions win, step limit, concurrency, audit-first — and every
// attempt lands in the INSERT-only apply_events trail.
package apply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"consize/internal/store"
)

// Config tunes the guardrails.
type Config struct {
	// StepLimit is the maximum fraction of the current value an apply
	// may change in one step (0.30 = 30%, per ADR-004). Larger
	// reductions step down over multiple applies.
	StepLimit float64
	// ConcurrentApplies is the global in-flight cap (applied, unverified).
	ConcurrentApplies int
	// AutoApplyLabel is the namespace label whose value must be
	// "enabled" for mode=auto applies.
	AutoApplyLabel string
}

// DefaultConfig returns the shipped guardrail settings.
func DefaultConfig() Config {
	return Config{StepLimit: 0.30, ConcurrentApplies: 1, AutoApplyLabel: "consize.savings.dev/auto-apply"}
}

// Patcher touches the cluster. The only write surface Consize has.
type Patcher interface {
	// PatchDeployment applies one resource's request+limit change to
	// every container of the deployment, proportionally to each
	// container's current share. Guarded by resourceVersion: conflicts
	// are retried on a fresh read.
	PatchDeployment(ctx context.Context, namespace, name string, d store.Diff) error
	// ReadResources returns the deployment's current aggregate request
	// and limit totals for one resource kind (summed across
	// containers). Rollback uses it to target the pre-apply values
	// absolutely instead of as a delta onto drifted state.
	ReadResources(ctx context.Context, namespace, name, kind string) (req, lim int64, err error)
	// Health reports cluster reachability (readiness probe).
	Health(ctx context.Context) error
}

// GuardError is a blocked apply with the reasons the guardrails found.
type GuardError struct {
	Reasons []string
}

func (e *GuardError) Error() string {
	return fmt.Sprintf("apply blocked: %v", e.Reasons)
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
}

// Service evaluates guardrails and applies recommendations.
type Service struct {
	st      store.Store
	patcher Patcher
	cfg     Config
	log     *slog.Logger
}

// NewService wires the apply engine.
func NewService(st store.Store, patcher Patcher, cfg Config) *Service {
	return &Service{st: st, patcher: patcher, cfg: cfg, log: slog.Default()}
}

// Health reports readiness of the apply engine: the store (audit trail
// must be writable, ADR-008) and the cluster write path. The API's
// readyz gates applies on both.
func (s *Service) Health(ctx context.Context) error {
	if err := s.st.Health(ctx); err != nil {
		return err
	}
	return s.patcher.Health(ctx)
}

// Apply runs the guardrail pipeline and, for non-dry modes, patches.
//
//  1. store health            — fail-safe: never apply without the audit trail
//  2. recommendation state    — pending only
//  3. exclusions win          — exclude label, data-loss-risk, protected ns
//  4. mode policy             — auto needs the namespace auto-apply label
//  5. step limit              — never more than StepLimit per apply
//  6. concurrency             — one in-flight apply per namespace, global cap
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

	// 3. Exclusions win.
	wl, err := s.st.GetWorkload(ctx, rec.WorkloadID)
	if err != nil {
		return res, ErrNotFound
	}
	blocked := guardExclusions(wl)
	if len(blocked) > 0 {
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
		if wl.Labels[s.cfg.AutoApplyLabel] != "enabled" {
			return res, &GuardError{Reasons: []string{
				fmt.Sprintf("namespace %s is not labeled %s=enabled", wl.Namespace, s.cfg.AutoApplyLabel)}}
		}
	default:
		return res, &GuardError{Reasons: []string{"mode must be dry_run | approved | auto"}}
	}

	// 5. Step limit: compute the diff for this step; larger reductions
	// become follow-up recommendations (stepping continues apply by apply).
	diff, steps, followUp, err := s.stepPlan(rec)
	if err != nil {
		return res, err
	}
	res.Diff = diff
	res.StepNumber = steps.completed + 1
	res.TotalSteps = steps.total

	// 6. Concurrency.
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

	// Real apply: patch, then record.
	if err := s.patcher.PatchDeployment(ctx, wl.Namespace, wl.Name, diff); err != nil {
		return res, fmt.Errorf("patch %s/%s: %w", wl.Namespace, wl.Name, err)
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

// Rollback restores a deployment to an applied event's pre-apply values
// and records the inverse as a reverted event. The verifier calls this
// on a FAIL verdict.
//
// The restore is an ABSOLUTE write, not a delta onto the live state:
// the patcher distributes `Proposed − Current` over the containers'
// live requests, so expressing the target as `Current = live` makes
// the totals land exactly on the pre-apply values even when the live
// state drifted during the window (a regression like the one the
// verifier just caught is exactly such a drift — a swapped-diff inverse
// would land on `live + (preApply − applied)`, restoring nothing).
func (s *Service) Rollback(ctx context.Context, event store.ApplyEvent) error {
	wl, err := s.st.GetWorkload(ctx, event.WorkloadID)
	if err != nil {
		return err
	}
	liveReq, liveLim, err := s.patcher.ReadResources(ctx, wl.Namespace, wl.Name, event.Diff.Resource)
	if err != nil {
		return fmt.Errorf("rollback read %s/%s: %w", wl.Namespace, wl.Name, err)
	}
	inv := store.Diff{
		Resource:      event.Diff.Resource,
		CurrentReq:    liveReq,
		ProposedReq:   event.Diff.CurrentReq,
		CurrentLimit:  liveLim,
		ProposedLimit: event.Diff.CurrentLimit,
	}
	if err := s.patcher.PatchDeployment(ctx, wl.Namespace, wl.Name, inv); err != nil {
		return fmt.Errorf("rollback patch %s/%s: %w", wl.Namespace, wl.Name, err)
	}
	_, err = s.st.CreateApplyEvent(ctx, store.ApplyEvent{
		RecommendationID: event.RecommendationID, WorkloadID: event.WorkloadID,
		Actor: "verifier", Mode: event.Mode,
		Result: store.EventReverted, Diff: inv,
		StepNumber: event.StepNumber, TotalSteps: event.TotalSteps,
	})
	if err != nil {
		return err
	}
	return s.st.SetRecommendationStatus(ctx, event.RecommendationID, store.StatusRolled)
}

// guardExclusions returns the exclusion reasons (empty = eligible).
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

// stepPlan computes the diff for this apply and, when the reduction
// exceeds the step limit, the follow-up recommendation that continues it.
func (s *Service) stepPlan(rec store.Recommendation) (store.Diff, stepInfo, *store.Recommendation, error) {
	targetReq := rec.ProposedValue
	targetLim := rec.ProposedLimit
	curReq := rec.CurrentValue
	curLim := rec.CurrentLimit
	if targetReq >= curReq && targetLim >= curLim {
		return store.Diff{}, stepInfo{}, nil, errors.New("nothing to reduce (downsize-only)")
	}

	stepReq, stepLim, total := s.stepValues(curReq, curLim, targetReq, targetLim)
	diff := store.Diff{
		Resource:   rec.Resource,
		CurrentReq: curReq, ProposedReq: stepReq,
		CurrentLimit: curLim, ProposedLimit: stepLim,
	}

	// Remainder becomes a follow-up when the step can't reach the target.
	var followUp *store.Recommendation
	if stepReq != targetReq || stepLim != targetLim {
		next := rec
		next.ID = 0
		next.Status = store.StatusPending
		next.CurrentValue = stepReq
		next.ProposedValue = targetReq
		next.CurrentLimit = stepLim
		next.ProposedLimit = targetLim
		// The remainder's savings scale with its request reduction share.
		next.SavingsMonthly = savingsOf(rec.SavingsMonthly, stepReq, targetReq, curReq)
		followUp = &next
	}
	return diff, stepInfo{completed: 0, total: total}, followUp, nil
}

// stepValues applies the step limit to (request, limit) toward targets.
func (s *Service) stepValues(curReq, curLim, targetReq, targetLim int64) (req, lim int64, totalSteps int) {
	limMult := s.cfg.StepLimit
	if limMult <= 0 {
		limMult = 0.30
	}
	req = stepToward(curReq, targetReq, limMult)
	lim = stepToward(curLim, targetLim, limMult)

	total := 1
	for c := req; c != targetReq && c > 0; c = stepToward(c, targetReq, limMult) {
		total++
		if total > 100 {
			break
		}
	}
	return req, lim, total
}

// stepToward moves current one step toward target (down only).
func stepToward(current, target int64, frac float64) int64 {
	if target >= current {
		return current
	}
	step := int64(math.Floor(float64(current) * frac))
	next := current - step
	if next < target {
		return target
	}
	return next
}

// savingsOf scales a recommendation's total savings to the portion of
// the reduction this step leaves for the follow-up.
func savingsOf(total float64, stepReq, finalReq, curReq int64) float64 {
	if curReq == finalReq {
		return 0
	}
	return total * float64(stepReq-finalReq) / float64(curReq-finalReq)
}

type stepInfo struct {
	completed int
	total     int
}

func actorOr(actor, def string) string {
	if actor == "" {
		return def
	}
	return actor
}
