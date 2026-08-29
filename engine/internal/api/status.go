package api

import (
	"net/http"
	"time"

	"consize/internal/config"
)

const (
	defaultDataStaleAfter = 2 * time.Hour
	defaultVerifyWindow   = 24 * time.Hour
)

type systemStatusResponse struct {
	Status                 string     `json:"status"`
	GeneratedAt            time.Time  `json:"generated_at"`
	LatestUsageBucket      *time.Time `json:"latest_usage_bucket"`
	TelemetryAgeSeconds    *int64     `json:"telemetry_age_seconds"`
	StaleAfterSeconds      int64      `json:"stale_after_seconds"`
	VerifyWindowSeconds    int64      `json:"verify_window_seconds"`
	Workloads              int        `json:"workloads"`
	PendingRecommendations int        `json:"pending_recommendations"`
	InFlightApplies        int        `json:"in_flight_applies"`
	VerificationDue        int        `json:"verification_due"`
	NextVerificationDueAt  *time.Time `json:"next_verification_due_at,omitempty"`
	Store                  string     `json:"store"`
	Messages               []string   `json:"messages"`
}

func (s *Server) systemStatus(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	staleAfter := config.Duration("CONSIZE_DATA_STALE_AFTER", defaultDataStaleAfter)
	verifyWindow := config.Duration("CONSIZE_VERIFY_WINDOW", defaultVerifyWindow)
	out := systemStatusResponse{
		Status:              "healthy",
		GeneratedAt:         now,
		StaleAfterSeconds:   int64(staleAfter.Seconds()),
		VerifyWindowSeconds: int64(verifyWindow.Seconds()),
		Store:               "ok",
		Messages:            []string{},
	}

	if err := s.store.Health(r.Context()); err != nil {
		out.Status = "unavailable"
		out.Store = "unavailable"
		out.Messages = append(out.Messages, "store unavailable: "+err.Error())
		writeJSON(w, http.StatusServiceUnavailable, out)
		return
	}

	workloads, err := s.store.ListWorkloads(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out.Workloads = len(workloads)

	if _, pending, err := s.store.SavingsSummary(r.Context()); err != nil {
		writeErr(w, err)
		return
	} else {
		out.PendingRecommendations = pending
	}

	if active, err := s.store.ActiveApplyCount(r.Context()); err != nil {
		writeErr(w, err)
		return
	} else {
		out.InFlightApplies = active
	}

	if latest, ok, err := s.store.LatestBucketTime(r.Context()); err != nil {
		writeErr(w, err)
		return
	} else if ok {
		out.LatestUsageBucket = &latest
		age := int64(now.Sub(latest).Seconds())
		if age < 0 {
			age = 0
		}
		out.TelemetryAgeSeconds = &age
		if time.Duration(age)*time.Second > staleAfter {
			out.Status = "degraded"
			out.Messages = append(out.Messages, "telemetry is stale; collector may not be writing fresh buckets")
		}
	} else if out.Workloads > 0 {
		out.Status = "degraded"
		out.Messages = append(out.Messages, "no telemetry buckets found for managed workloads")
	} else {
		out.Status = "empty"
		out.Messages = append(out.Messages, "no workloads collected yet")
	}

	unverified, err := s.store.AppliedEventsUnverified(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	for _, e := range unverified {
		if e.CreatedAt.IsZero() {
			continue
		}
		dueAt := e.CreatedAt.Add(verifyWindow)
		if out.NextVerificationDueAt == nil || dueAt.Before(*out.NextVerificationDueAt) {
			next := dueAt
			out.NextVerificationDueAt = &next
		}
		if !now.Before(dueAt) {
			out.VerificationDue++
		}
	}
	if out.VerificationDue > 0 && out.Status == "healthy" {
		out.Status = "attention"
	}
	if out.VerificationDue > 0 {
		out.Messages = append(out.Messages, "automatic safety verification is ready to run")
	} else if out.InFlightApplies > 0 {
		out.Messages = append(out.Messages, "an applied change is waiting for its verification window")
	}

	writeJSON(w, http.StatusOK, out)
}
