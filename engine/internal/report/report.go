package report

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"consize/internal/alert"
	"consize/internal/store"
)

const StoreConfigKey = "weekly_report"

type Config struct {
	Enabled   bool `json:"enabled"`
	RangeDays int  `json:"range_days"`
}

func DefaultConfig() Config {
	return Config{Enabled: false, RangeDays: 7}
}

func ParseConfig(raw string) (Config, error) {
	cfg := DefaultConfig()
	if strings.TrimSpace(raw) == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return Config{}, err
	}
	return NormalizeConfig(cfg)
}

func NormalizeConfig(cfg Config) (Config, error) {
	switch cfg.RangeDays {
	case 0:
		cfg.RangeDays = 7
	case 7, 14, 30:
	default:
		return Config{}, fmt.Errorf("range_days must be one of 7, 14, or 30")
	}
	return cfg, nil
}

type Summary struct {
	GeneratedAt                      time.Time        `json:"generated_at"`
	From                             time.Time        `json:"from"`
	To                               time.Time        `json:"to"`
	RangeDays                        int              `json:"range_days"`
	ProjectedMonthlySavings          float64          `json:"projected_monthly_savings"`
	PendingRecommendations           int              `json:"pending_recommendations"`
	RealizedThisPeriodMonthlySavings float64          `json:"realized_this_period_monthly_savings"`
	VerifiedApplies                  int              `json:"verified_applies"`
	Rollbacks                        int              `json:"rollbacks"`
	FailedVerifications              int              `json:"failed_verifications"`
	InconclusiveVerifications        int              `json:"inconclusive_verifications"`
	LatestUsageBucket                *time.Time       `json:"latest_usage_bucket,omitempty"`
	DataQuality                      []string         `json:"data_quality"`
	TopPendingRecommendations        []Recommendation `json:"top_pending_recommendations"`
	RecentRollbacks                  []ApplyEvent     `json:"recent_rollbacks"`
}

type Recommendation struct {
	ID             int64     `json:"id"`
	WorkloadID     int64     `json:"workload_id"`
	WorkloadName   string    `json:"workload_name"`
	Namespace      string    `json:"namespace"`
	Resource       string    `json:"resource"`
	Current        string    `json:"current"`
	Proposed       string    `json:"proposed"`
	SavingsMonthly float64   `json:"savings_monthly"`
	CreatedAt      time.Time `json:"created_at"`
}

type ApplyEvent struct {
	ID               int64     `json:"id"`
	RecommendationID int64     `json:"recommendation_id"`
	WorkloadName     string    `json:"workload_name"`
	Namespace        string    `json:"namespace"`
	Resource         string    `json:"resource"`
	Change           string    `json:"change"`
	Actor            string    `json:"actor"`
	CreatedAt        time.Time `json:"created_at"`
}

func Build(ctx context.Context, st store.Store, now time.Time, rangeDays int) (Summary, error) {
	cfg, err := NormalizeConfig(Config{RangeDays: rangeDays})
	if err != nil {
		return Summary{}, err
	}
	now = now.UTC()
	from := now.AddDate(0, 0, -cfg.RangeDays)
	out := Summary{
		GeneratedAt:               now,
		From:                      from,
		To:                        now,
		RangeDays:                 cfg.RangeDays,
		DataQuality:               []string{},
		TopPendingRecommendations: []Recommendation{},
		RecentRollbacks:           []ApplyEvent{},
	}

	recs, _, err := st.ListRecommendations(ctx, nil, "", 0, 0)
	if err != nil {
		return Summary{}, err
	}
	events, err := st.ListApplyEvents(ctx, nil, "")
	if err != nil {
		return Summary{}, err
	}
	verifs, err := st.ListVerificationRuns(ctx, nil)
	if err != nil {
		return Summary{}, err
	}
	workloads, err := st.ListWorkloads(ctx)
	if err != nil {
		return Summary{}, err
	}
	if latest, ok, err := st.LatestBucketTime(ctx); err != nil {
		return Summary{}, err
	} else if ok {
		latest = latest.UTC()
		out.LatestUsageBucket = &latest
		if now.Sub(latest) > 2*time.Hour {
			out.DataQuality = append(out.DataQuality, "Telemetry has not produced a fresh bucket in more than 2 hours.")
		}
	} else {
		out.DataQuality = append(out.DataQuality, "No telemetry buckets have been collected yet.")
	}

	recsByID := map[int64]store.Recommendation{}
	for _, rec := range recs {
		recsByID[rec.ID] = rec
		if rec.Status == store.StatusPending {
			out.ProjectedMonthlySavings += rec.SavingsMonthly
			out.PendingRecommendations++
			if len(out.TopPendingRecommendations) < 5 {
				out.TopPendingRecommendations = append(out.TopPendingRecommendations, recView(rec))
			}
		}
	}

	workloadsByID := map[int64]store.Workload{}
	for _, wl := range workloads {
		workloadsByID[wl.ID] = wl
	}
	eventsByID := map[int64]store.ApplyEvent{}
	for _, ev := range events {
		eventsByID[ev.ID] = ev
		if ev.CreatedAt.UTC().Before(from) || ev.CreatedAt.UTC().After(now) {
			continue
		}
		if ev.Result == store.EventReverted {
			out.Rollbacks++
			if len(out.RecentRollbacks) < 5 {
				out.RecentRollbacks = append(out.RecentRollbacks, eventView(ev, workloadsByID[ev.WorkloadID]))
			}
		}
	}

	seenVerifiedRecommendation := map[int64]bool{}
	for _, vr := range verifs {
		if vr.CreatedAt.UTC().Before(from) || vr.CreatedAt.UTC().After(now) {
			continue
		}
		switch vr.Verdict {
		case store.VerdictPassed:
			out.VerifiedApplies++
			ev, ok := eventsByID[vr.ApplyEventID]
			if !ok || seenVerifiedRecommendation[ev.RecommendationID] {
				continue
			}
			rec, ok := recsByID[ev.RecommendationID]
			if !ok {
				continue
			}
			seenVerifiedRecommendation[ev.RecommendationID] = true
			out.RealizedThisPeriodMonthlySavings += rec.SavingsMonthly
		case store.VerdictFailed:
			out.FailedVerifications++
		case store.VerdictInconclusive:
			out.InconclusiveVerifications++
		}
	}

	if len(out.DataQuality) == 0 {
		out.DataQuality = append(out.DataQuality, "Telemetry is fresh enough for reporting.")
	}
	return out, nil
}

func Event(s Summary) alert.Event {
	title := fmt.Sprintf("Consize %d-day savings report", s.RangeDays)
	summary := fmt.Sprintf("%s realized monthly savings, %s pending monthly savings across %d recommendations.",
		money(s.RealizedThisPeriodMonthlySavings), money(s.ProjectedMonthlySavings), s.PendingRecommendations)
	if lines := topPendingLines(s); len(lines) > 0 {
		summary += "\n" + strings.Join(lines, "\n")
	}
	return alert.Event{
		Title:    title,
		Summary:  summary,
		Status:   "firing",
		DedupKey: fmt.Sprintf("consize:report:%d:%s", s.RangeDays, s.To.Format("2006-01-02")),
		Labels: map[string]string{
			"alertname": "ConsizeWeeklyReport",
			"severity":  "info",
			"surface":   "reporting",
			"status":    "firing",
		},
		Annotations: map[string]string{
			"change":   fmt.Sprintf("verified applies: %d · rollbacks: %d", s.VerifiedApplies, s.Rollbacks),
			"rollback": fmt.Sprintf("verification failures: %d · inconclusive: %d", s.FailedVerifications, s.InconclusiveVerifications),
		},
	}
}

func recView(rec store.Recommendation) Recommendation {
	return Recommendation{
		ID:             rec.ID,
		WorkloadID:     rec.WorkloadID,
		WorkloadName:   rec.WorkloadName,
		Namespace:      rec.Namespace,
		Resource:       rec.Resource,
		Current:        current(rec),
		Proposed:       proposed(rec),
		SavingsMonthly: rec.SavingsMonthly,
		CreatedAt:      rec.CreatedAt,
	}
}

func eventView(ev store.ApplyEvent, wl store.Workload) ApplyEvent {
	return ApplyEvent{
		ID:               ev.ID,
		RecommendationID: ev.RecommendationID,
		WorkloadName:     wl.Name,
		Namespace:        wl.Namespace,
		Resource:         ev.Diff.Resource,
		Change:           diff(ev.Diff),
		Actor:            ev.Actor,
		CreatedAt:        ev.CreatedAt,
	}
}

func topPendingLines(s Summary) []string {
	if len(s.TopPendingRecommendations) == 0 {
		return []string{"top pending: none"}
	}
	lines := []string{"top pending:"}
	for _, rec := range s.TopPendingRecommendations {
		lines = append(lines, fmt.Sprintf("• %s/%s %s → %s (%s/mo)",
			rec.Namespace, rec.WorkloadName, rec.Current, rec.Proposed, money(rec.SavingsMonthly)))
	}
	return lines
}

func current(rec store.Recommendation) string {
	if rec.Resource == store.ResourceClass {
		return rec.ClassCurrent
	}
	return formatResource(rec.Resource, rec.CurrentValue)
}

func proposed(rec store.Recommendation) string {
	if rec.Resource == store.ResourceClass {
		return rec.ClassProposed
	}
	return formatResource(rec.Resource, rec.ProposedValue)
}

func diff(d store.Diff) string {
	if d.Resource == store.ResourceClass {
		return d.ClassCurrent + " → " + d.ClassProposed
	}
	return formatResource(d.Resource, d.CurrentReq) + " → " + formatResource(d.Resource, d.ProposedReq)
}

func formatResource(resource string, v int64) string {
	switch resource {
	case store.ResourceCPU:
		if v >= 1000 {
			return fmt.Sprintf("%.2f cores", float64(v)/1000)
		}
		return fmt.Sprintf("%dm", v)
	case store.ResourceMemory:
		mib := float64(v) / 1024 / 1024
		if mib >= 1024 {
			return fmt.Sprintf("%.2f GiB", mib/1024)
		}
		return fmt.Sprintf("%.0f MiB", mib)
	default:
		return fmt.Sprintf("%d", v)
	}
}

func money(v float64) string {
	return "$" + formatFloat(v)
}

func formatFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	parts := strings.Split(s, ".")
	intPart := parts[0]
	sign := ""
	if strings.HasPrefix(intPart, "-") {
		sign = "-"
		intPart = strings.TrimPrefix(intPart, "-")
	}
	groups := []string{}
	for len(intPart) > 3 {
		groups = append(groups, intPart[len(intPart)-3:])
		intPart = intPart[:len(intPart)-3]
	}
	groups = append(groups, intPart)
	for i, j := 0, len(groups)-1; i < j; i, j = i+1, j-1 {
		groups[i], groups[j] = groups[j], groups[i]
	}
	return sign + strings.Join(groups, ",") + "." + parts[1]
}
