// Package verifier compares SLIs over the post-apply window against a
// pre-apply baseline and decides: PASS (recommendation verified),
// FAIL (auto-rollback), or INCONCLUSIVE (explicit, never silence —
// ADR-006). One verdict per applied event, recorded in verification_runs.
package verifier

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"regexp"
	"strings"
	"time"

	"consize/internal/alert"
	"consize/internal/collector"
	"consize/internal/config"
	"consize/internal/store"
)

// Config tunes verification.
type Config struct {
	// Window is the base baseline/post comparison window. The effective
	// window scales by apply step: step 1 = 1x, step 2 = 2x, etc.
	Window time.Duration
	// SustainedMinutes is how long a rate must stay above its
	// threshold before it counts as a breach (5).
	SustainedMinutes int
	// ErrorExpr and P99Expr are optional app-level metric expressions
	// (default empty = signal disabled). The expression must include a
	// namespace selector of its own, e.g.
	// `http_requests_total{code=~"^5",namespace="<ns>"}` is NOT
	// supported — use a label that is already per-namespace or leave
	// these unset. Rate-of-change is applied automatically.
	ErrorExpr string
	P99Expr   string
}

// DefaultConfig returns the shipped settings.
func DefaultConfig() Config {
	return Config{Window: time.Hour, SustainedMinutes: 5}
}

// signalSpec is one SLI: how to query it and how to judge it.
type signalSpec struct {
	name  string
	kind  string // "rate" | "counter"
	mult  float64
	query func(scope queryScope) string
}

type queryScope struct {
	namespace string
	workload  string
}

// Service verifies applied events against Prometheus.
type Service struct {
	prom     collector.PrometheusClient
	st       store.Store
	rollback func(ctx context.Context, e store.ApplyEvent) error
	cfg      Config
	alert    *alert.Notifier
	log      *slog.Logger
}

// New wires the verifier. rollback is the apply engine's Rollback —
// the verifier never touches the cluster itself (one write surface).
func New(prom collector.PrometheusClient, st store.Store,
	rollback func(ctx context.Context, e store.ApplyEvent) error, cfg Config) *Service {
	return &Service{
		prom: prom, st: st, rollback: rollback, cfg: cfg,
		alert: alert.NewWithConfigProvider(st), log: slog.Default(),
	}
}

// Verdict is the outcome of one verification.
type Verdict struct {
	Failed       bool
	Inconclusive bool
	SLIs         map[string]any
	Thresholds   map[string]any
}

// String renders the verdict for logs and the API.
func (v Verdict) String() string {
	switch {
	case v.Failed:
		return store.VerdictFailed
	case v.Inconclusive:
		return store.VerdictInconclusive
	default:
		return store.VerdictPassed
	}
}

// signals builds the active SLI set from config.
func (s *Service) signals() []signalSpec {
	out := []signalSpec{
		{name: "throttling", kind: "rate", mult: 1.0, query: func(scope queryScope) string {
			return fmt.Sprintf(`sum by (namespace) (rate(container_cpu_cfs_throttled_seconds_total{%s}[5m]))`, scope.containerMatcher())
		}},
		{name: "oom_killed", kind: "counter", mult: 0, query: func(scope queryScope) string {
			return fmt.Sprintf(`sum by (namespace) (increase(container_oom_events_total{%s}[5m]))`, scope.containerMatcher())
		}},
		{name: "restarts", kind: "counter", mult: 0, query: func(scope queryScope) string {
			return fmt.Sprintf(`sum by (namespace) (increase(kube_pod_container_status_restarts_total{%s}[5m]))`, scope.containerMatcher())
		}},
		{name: "evictions", kind: "counter", mult: 0, query: func(scope queryScope) string {
			return fmt.Sprintf(`sum by (namespace) (increase(kube_pod_status_reason{%s,reason="Evicted"}[5m]))`, scope.podMatcher())
		}},
	}
	if s.cfg.ErrorExpr != "" {
		out = append(out, signalSpec{name: "error_rate", kind: "rate", mult: 1.5, query: func(_ queryScope) string {
			return fmt.Sprintf(`sum by (namespace) (rate(%s[5m]))`, s.cfg.ErrorExpr)
		}})
	}
	if s.cfg.P99Expr != "" {
		out = append(out, signalSpec{name: "p99_latency", kind: "rate", mult: 1.3, query: func(_ queryScope) string {
			return fmt.Sprintf(`sum by (namespace) (rate(%s[5m]))`, s.cfg.P99Expr)
		}})
	}
	return out
}

// podMatcher scopes built-in Kubernetes SLIs to the Deployment that was
// changed. Consize currently manages Deployment workloads; their pods are
// named <deployment>-<pod-template-hash>-<pod-id>. This keeps an unrelated
// CrashLoopBackOff in the same namespace from rolling back a healthy apply.
func (q queryScope) podMatcher() string {
	ns := fmt.Sprintf(`namespace=%q`, q.namespace)
	if q.workload == "" {
		return ns
	}
	podRegex := "^" + regexp.QuoteMeta(q.workload) + `-[^-]+-[^-]+$`
	return fmt.Sprintf(`%s,pod=~%q`, ns, podRegex)
}

func (q queryScope) containerMatcher() string {
	return q.podMatcher() + `,container!="POD",container!=""`
}

// Verify compares SLIs across the baseline and post windows for one
// applied event, records the verification_run, and rolls back on FAIL.
// Database class events route to the store-driven DB path (VerifyDB);
// everything else is the k8s Prometheus path — a class event must never
// be judged against container metrics or rolled back through the
// deployment patcher (one write surface per kind).
func (s *Service) Verify(ctx context.Context, event store.ApplyEvent) (Verdict, error) {
	if event.Diff.Resource == store.ResourceClass {
		return s.VerifyDB(ctx, event)
	}
	return s.verifyK8s(ctx, event)
}

func (s *Service) verifyK8s(ctx context.Context, event store.ApplyEvent) (Verdict, error) {
	wl, err := s.st.GetWorkload(ctx, event.WorkloadID)
	if err != nil {
		return Verdict{}, fmt.Errorf("load workload %d: %w", event.WorkloadID, err)
	}
	applyTime := event.CreatedAt.UTC()
	window := config.StepScaledDuration(s.cfg.Window, event.StepNumber)
	baseStart, baseEnd := applyTime.Add(-window), applyTime
	postStart, postEnd := applyTime, time.Now().UTC()

	v := Verdict{SLIs: map[string]any{}, Thresholds: map[string]any{}}
	var judged, unavailable, inconclusive int
	scope := queryScope{namespace: wl.Namespace, workload: wl.Name}
	for _, sig := range s.signals() {
		ev, _, err := s.evaluate(ctx, sig, scope, baseStart, baseEnd, postStart, postEnd)
		if err != nil {
			return v, err
		}
		v.SLIs[sig.name] = ev
		switch ev["verdict"] {
		case "failed":
			v.Failed = true
			judged++
		case "inconclusive":
			inconclusive++
			v.Inconclusive = true
		case "passed":
			judged++
		default: // unavailable — signal not emitted by this cluster
			unavailable++
		}
	}
	// Never silence: a verification where nothing could be judged is
	// inconclusive, not a pass (ADR-006).
	if judged == 0 && inconclusive == 0 {
		v.Inconclusive = true
	}
	v.Thresholds = map[string]any{
		"window":            window.String(),
		"base_window":       s.cfg.Window.String(),
		"step_number":       event.StepNumber,
		"sustained_minutes": s.cfg.SustainedMinutes,
		"error_mult":        1.5, "p99_mult": 1.3,
	}

	return v, s.recordAndAct(ctx, event, wl, v, baseStart, baseEnd, postStart, postEnd)
}

// recordAndAct is the shared tail of both verification paths: persist
// the verification_run (INSERT-only), log, alert and roll back on FAIL.
// The FAIL verdict stands even when the rollback itself fails — the
// alert escalates to manual intervention.
func (s *Service) recordAndAct(ctx context.Context, event store.ApplyEvent, wl store.Workload, v Verdict,
	baseStart, baseEnd, postStart, postEnd time.Time) error {

	if err := s.st.CreateVerificationRun(ctx, store.VerificationRun{
		ApplyEventID:  event.ID,
		BaselineStart: baseStart, BaselineEnd: baseEnd,
		PostStart: postStart, PostEnd: postEnd,
		Verdict:    v.String(),
		SLIs:       v.SLIs,
		Thresholds: v.Thresholds,
	}); err != nil {
		return fmt.Errorf("record verification: %w", err)
	}

	s.log.Info("verification", "event", event.ID, "workload", wl.Namespace+"/"+wl.Name,
		"verdict", v.String(), "slis", v.SLIs)

	if v.Failed {
		s.alert.NotifyEvent(ctx, verificationFailedAlert(event, wl, v))
		if err := s.rollback(ctx, event); err != nil {
			// The FAIL verdict stands; the human must intervene.
			s.alert.NotifyEvent(ctx, rollbackFailedAlert(event, wl, err))
			s.log.Error("rollback failed", "event", event.ID, "err", err)
		}
	}
	return nil
}

func verificationFailedAlert(event store.ApplyEvent, wl store.Workload, v Verdict) alert.Event {
	workload := wl.Namespace + "/" + wl.Name
	annotations := map[string]string{
		"change":   changeSummary(event.Diff),
		"rollback": "automatic rollback started",
	}
	if sig := failedSignal(v.SLIs); sig != "" {
		annotations["failed_signal"] = sig
	}
	if u := dashboardURL(wl.ID); u != "" {
		annotations["dashboard_url"] = u
	}
	return alert.Event{
		Title:       "Consize verification failed",
		Summary:     fmt.Sprintf("%s failed verification for apply #%d%s. Rolling back now.", workload, event.ID, escalationLabel(wl)),
		Status:      "firing",
		DedupKey:    fmt.Sprintf("consize:%s:%d:verification-failed", workload, event.ID),
		Labels:      alertLabels("ConsizeVerificationFailed", "critical", event, wl),
		Annotations: annotations,
	}
}

func rollbackFailedAlert(event store.ApplyEvent, wl store.Workload, err error) alert.Event {
	workload := wl.Namespace + "/" + wl.Name
	annotations := map[string]string{
		"change":   changeSummary(event.Diff),
		"rollback": "automatic rollback failed: " + err.Error(),
	}
	if u := dashboardURL(wl.ID); u != "" {
		annotations["dashboard_url"] = u
	}
	return alert.Event{
		Title:       "Consize rollback failed",
		Summary:     fmt.Sprintf("%s rollback failed after apply #%d%s. Manual intervention required.", workload, event.ID, escalationLabel(wl)),
		Status:      "firing",
		DedupKey:    fmt.Sprintf("consize:%s:%d:rollback-failed", workload, event.ID),
		Labels:      alertLabels("ConsizeRollbackFailed", "critical", event, wl),
		Annotations: annotations,
	}
}

func alertLabels(name, severity string, event store.ApplyEvent, wl store.Workload) map[string]string {
	labels := map[string]string{
		"alertname": name,
		"severity":  severity,
		"namespace": wl.Namespace,
		"workload":  wl.Name,
		"resource":  event.Diff.Resource,
		"surface":   wl.Source,
		"status":    "firing",
	}
	if wl.TeamName != "" {
		labels["team"] = wl.TeamName
	}
	if wl.TeamOnCall != "" {
		labels["oncall"] = wl.TeamOnCall
	}
	return labels
}

func changeSummary(d store.Diff) string {
	if d.Resource == store.ResourceClass {
		return fmt.Sprintf("`%s` → `%s`", d.ClassCurrent, d.ClassProposed)
	}
	if d.CurrentLimit != 0 || d.ProposedLimit != 0 {
		return fmt.Sprintf("%s request `%d` → `%d`, limit `%d` → `%d`", d.Resource, d.CurrentReq, d.ProposedReq, d.CurrentLimit, d.ProposedLimit)
	}
	return fmt.Sprintf("%s request `%d` → `%d`", d.Resource, d.CurrentReq, d.ProposedReq)
}

func failedSignal(slis map[string]any) string {
	for name, raw := range slis {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if verdict, _ := m["verdict"].(string); verdict == "failed" {
			return name
		}
	}
	return ""
}

func dashboardURL(workloadID int64) string {
	base := os.Getenv("CONSIZE_DASHBOARD_URL")
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/workloads/%d", strings.TrimRight(base, "/"), workloadID)
}

// escalationLabel keeps the team boundary visible in every failed
// verification alert. The existing webhook remains the delivery channel;
// a later PagerDuty/Slack-router integration can use the same TeamOnCall
// value without changing the safety engine's verdict semantics.
func escalationLabel(wl store.Workload) string {
	if wl.TeamName == "" {
		return " — team unassigned"
	}
	if wl.TeamOnCall == "" {
		return " — team " + wl.TeamName + ", on-call unassigned"
	}
	return " — team " + wl.TeamName + ", on-call " + wl.TeamOnCall
}

// evaluate judges one signal and returns its evidence map plus whether
// the signal was available at all (ok=false = metric not emitted).
func (s *Service) evaluate(ctx context.Context, sig signalSpec, scope queryScope,
	baseStart, baseEnd, postStart, postEnd time.Time) (map[string]any, bool, error) {

	ev := map[string]any{"signal": sig.name}
	base, err := s.fetch(ctx, sig.query(scope), baseStart, baseEnd)
	if err != nil {
		return nil, false, err
	}
	post, err := s.fetch(ctx, sig.query(scope), postStart, postEnd)
	if err != nil {
		return nil, false, err
	}
	if len(base) == 0 && len(post) == 0 {
		ev["verdict"] = "unavailable"
		return ev, false, nil
	}
	if len(base) == 0 || len(post) == 0 {
		ev["verdict"] = "inconclusive"
		ev["reason"] = "data missing in one window"
		return ev, true, nil
	}

	switch sig.kind {
	case "rate":
		baseline := median(base)
		threshold := baseline * sig.mult
		run := longestRun(post, threshold)
		ev["baseline_median"] = baseline
		ev["threshold"] = threshold
		ev["post_above_threshold"] = run.samples
		ev["longest_breach_minutes"] = run.minutes
		if run.minutes >= s.cfg.SustainedMinutes {
			ev["verdict"] = "failed"
			ev["reason"] = fmt.Sprintf("%d samples above %.4g sustained %d min",
				run.samples, threshold, run.minutes)
		} else {
			ev["verdict"] = "passed"
		}
	case "counter":
		baseTotal, postTotal := sum(base), sum(post)
		ev["baseline_total"], ev["post_total"] = baseTotal, postTotal
		if postTotal > baseTotal {
			ev["verdict"] = "failed"
			ev["reason"] = fmt.Sprintf("new events: %v post vs %v baseline", postTotal, baseTotal)
		} else {
			ev["verdict"] = "passed"
		}
	}
	return ev, true, nil
}

// fetch pulls one window at 1-minute resolution and returns the finite
// sample values (summed across the namespace's series).
func (s *Service) fetch(ctx context.Context, query string, start, end time.Time) ([]float64, error) {
	series, err := s.prom.QueryRange(ctx, query, start, end, time.Minute)
	if err != nil {
		return nil, err
	}
	var out []float64
	for _, ser := range series {
		for _, p := range ser.Points {
			if math.IsNaN(p.Value) || math.IsInf(p.Value, 0) {
				continue
			}
			out = append(out, p.Value)
		}
	}
	return out, nil
}

// run is the longest streak of consecutive samples above a threshold.
type run struct {
	samples int
	minutes int
}

func longestRun(values []float64, threshold float64) run {
	best, cur := 0, 0
	for _, v := range values {
		if v > threshold {
			cur++
			if cur > best {
				best = cur
			}
		} else {
			cur = 0
		}
	}
	return run{samples: best, minutes: best} // 1-minute samples
}

func median(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vs...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

func sum(vs []float64) float64 {
	var t float64
	for _, v := range vs {
		t += v
	}
	return t
}
