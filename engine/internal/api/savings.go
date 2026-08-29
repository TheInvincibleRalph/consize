package api

import (
	"context"
	"net/http"

	"consize/internal/store"
)

// ownerSavings is one owner's projected and realized monthly savings.
type ownerSavings struct {
	ProjectedMonthly float64 `json:"projected_monthly"`
	RealizedMonthly  float64 `json:"realized_monthly"`
}

// savings serves GET /api/v1/savings: the M1 projected figures plus the
// M3.5 realized and per-owner breakdowns. Projected and realized are
// never conflated: projected sums PENDING recommendations; realized sums
// recommendations whose latest apply event is still applied and whose
// verification verdict passed. A reverted, dry-run, failed, or inconclusive
// change is not realized, however much it promised.
func (s *Server) savings(w http.ResponseWriter, r *http.Request) {
	total, n, err := s.store.SavingsSummary(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	// The savings figure is only as good as the price table; expose it.
	prices, err := s.prices.Prices(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	realized, byOwner, err := s.savingsRealized(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"projected_monthly_savings": total,
		"active_recommendations":    n,
		"realized_monthly":          realized,
		"realized_yearly":           realized * 12,
		"by_owner":                  byOwner,
		"price_table":               prices,
	})
}

// savingsRealized computes realized savings and the by-owner breakdown.
// owner is the workload's Labels["owner"], "unassigned" when unset.
func (s *Server) savingsRealized(ctx context.Context) (float64, map[string]ownerSavings, error) {
	recs, _, err := s.store.ListRecommendations(ctx, nil, "", 0, 0)
	if err != nil {
		return 0, nil, err
	}
	events, err := s.store.ListApplyEvents(ctx, nil, "")
	if err != nil {
		return 0, nil, err
	}
	verifs, err := s.store.ListVerificationRuns(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	// Apply events come newest-first from the store, so the first event
	// seen per recommendation is its latest.
	latest := map[int64]store.ApplyEvent{}
	for _, e := range events {
		if _, seen := latest[e.RecommendationID]; !seen {
			latest[e.RecommendationID] = e
		}
	}
	passedApply := map[int64]bool{}
	for _, v := range verifs {
		if v.Verdict == store.VerdictPassed {
			passedApply[v.ApplyEventID] = true
		}
	}

	ownerOf := map[int64]string{}
	ws, err := s.store.ListWorkloads(ctx)
	if err != nil {
		return 0, nil, err
	}
	for _, w := range ws {
		if o := w.Labels["owner"]; o != "" {
			ownerOf[w.ID] = o
		} else {
			ownerOf[w.ID] = "unassigned"
		}
	}

	var realized float64
	byOwner := map[string]ownerSavings{}
	for _, rec := range recs {
		owner := ownerOf[rec.WorkloadID]
		if owner == "" {
			owner = "unassigned"
		}
		o := byOwner[owner]
		if rec.Status == store.StatusPending {
			o.ProjectedMonthly += rec.SavingsMonthly
		}
		latestEvent, hasLatestEvent := latest[rec.ID]
		if hasLatestEvent && latestEvent.Result == store.EventApplied && passedApply[latestEvent.ID] {
			o.RealizedMonthly += rec.SavingsMonthly
			realized += rec.SavingsMonthly
		}
		byOwner[owner] = o
	}
	return realized, byOwner, nil
}
