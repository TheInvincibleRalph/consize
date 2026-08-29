// Package api exposes Consize's HTTP API: workloads, recommendations,
// savings (M1), and the M2 safety surface — apply, the apply_events
// trail, and verification runs.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"consize/internal/apply"
	"consize/internal/auth"
	"consize/internal/dbapply"
	"consize/internal/pricing"
	"consize/internal/store"
	"consize/ui"
)

// Options are the additive API knobs (ADR-037). Zero Options — the
// signature every pre-auth caller uses — means auth is disabled and the
// API behaves exactly as before.
type Options struct {
	// Auth is the identity provider. nil disables authentication: no
	// login surface, no session checks, reads and writes open (the demo /
	// embedded-SPA build).
	Auth *auth.Service
	// CookieSecure marks the session cookie Secure (set it behind TLS).
	CookieSecure bool
}

// Server is the HTTP API.
type Server struct {
	store        store.Store
	prices       pricing.Service
	applier      *apply.Service   // nil = k8s apply endpoints return 503
	dbApplier    *dbapply.Service // nil = DB apply endpoints return 503
	authSvc      *auth.Service    // nil = auth disabled (ADR-037)
	cookieSecure bool
}

// New builds the router. Handlers only depend on interfaces, so tests
// mount it over the in-memory store. Either engine may be nil when the
// API has no matching write identity (dev/demo): read endpoints still
// work, that kind's apply endpoints answer 503 rather than lying about
// availability. The route picks the engine from the recommendation's
// resource (ADR-030): class → DB engine, cpu/memory → k8s engine.
//
// Options are variadic so every existing call site keeps compiling with
// auth disabled; pass Options{Auth: ...} to enable ADR-037 enforcement.
func New(st store.Store, pr pricing.Service, applier *apply.Service, dbApplier *dbapply.Service, opts ...Options) http.Handler {
	s := &Server{store: st, prices: pr, applier: applier, dbApplier: dbApplier}
	if len(opts) > 0 {
		s.authSvc = opts[0].Auth
		s.cookieSecure = opts[0].CookieSecure
	}
	// The middleware receives a nil Authenticator interface when auth is
	// disabled — NOT a nil *auth.Service boxed into the interface, which
	// would fail the nil check and enforce auth anyway.
	var authHandle auth.Authenticator
	if s.authSvc != nil {
		authHandle = s.authSvc
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", s.healthz)
	r.Get("/readyz", s.readyz)

	r.Route("/api/v1", func(r chi.Router) {
		// Auth surface (ADR-037). Login is public; logout revokes whatever
		// session cookie the caller holds; setup is the first-run wizard's
		// endpoint (creates the first admin while the users table is empty);
		// me reports the session and is deliberately NOT behind RequireUser
		// — its 401 body carries needs_setup so the wizard can advertise
		// itself only while no user exists. When auth is disabled
		// (Options.Auth == nil) the RequireUser/RequireRole middleware below
		// pass everything through, and me answers auth_enabled:false — the
		// embedded-SPA demo keeps working.
		r.Post("/auth/login", s.login)
		r.Post("/auth/logout", s.logout)
		r.Post("/auth/setup", s.setupAdmin)
		r.Get("/auth/me", s.me)

		// Reads: any authenticated user (viewer included).
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireUser(authHandle))
			r.Get("/teams", s.listTeams)
			r.Get("/workloads", s.listWorkloads)
			r.Get("/workloads/{id}", s.getWorkload)
			r.Get("/workloads/{id}/series", s.workloadSeries)
			r.Get("/recommendations", s.listRecommendations)
			r.Get("/savings", s.savings)
			r.Get("/system/status", s.systemStatus)
			r.Get("/alerting/config", s.getAlertingConfig)
			r.Get("/integrations/github", s.getGitHubIntegration)
			r.Get("/reports/config", s.getReportConfig)
			r.Get("/reports/savings", s.getSavingsReport)
			r.Get("/applies", s.listApplies)
			r.Get("/verification-runs", s.listVerificationRuns)
			r.Get("/cost-opportunities", s.listCostOpportunities)
		})

		// Writes: operator or admin only, server-verified (M4 AC — the
		// client cannot self-report an identity).
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireUser(authHandle))
			r.Use(auth.RequireRole(authHandle, store.RoleOperator))
			r.Post("/recommendations/{id}/apply", s.applyRecommendation)
			r.Post("/recommendations/{id}/iac-pr", s.prepareRecommendationIaCPullRequest)
			r.Post("/cost-opportunities/scan", s.scanCostOpportunities)
			r.Post("/cost-opportunities/{id}/iac-pr", s.prepareIaCPullRequest)
		})

		// Ownership changes are governance configuration, not routine
		// operation: only admins may create teams or change the escalation
		// target associated with a workload (ADR-043).
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireUser(authHandle))
			r.Use(auth.RequireRole(authHandle, store.RoleAdmin))
			r.Post("/teams", s.createTeam)
			r.Patch("/teams/{id}", s.updateTeam)
			r.Put("/workloads/{id}/team", s.assignWorkloadTeam)
			r.Delete("/workloads/{id}/team", s.unassignWorkloadTeam)
			r.Put("/alerting/config", s.putAlertingConfig)
			r.Post("/alerting/test", s.testAlertingConfig)
			r.Put("/integrations/github", s.putGitHubIntegration)
			r.Put("/reports/config", s.putReportConfig)
			r.Post("/reports/send", s.sendSavingsReport)
		})
	})

	// The embedded dashboard (package ui) is the last mount: it only sees
	// paths no API route matched. Unknown /api/* paths stay 404s instead
	// of falling back to the SPA, so a wrong endpoint reports itself
	// honestly.
	dashboard := ui.Handler()
	serveDashboard := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		dashboard.ServeHTTP(w, r)
	}
	r.Get("/", serveDashboard)
	r.Get("/*", serveDashboard)

	return r
}

// liveness: the process is up, no dependencies consulted.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readiness: the store (audit trail) must be reachable — applies must
// never run against a store that's down (fail-safe principle). When a
// cluster write identity is configured, the cluster must answer too.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Health(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "detail": err.Error()})
		return
	}
	if s.applier != nil {
		if err := s.applier.Health(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unavailable", "detail": "cluster: " + err.Error()})
			return
		}
	}
	if s.dbApplier != nil {
		if err := s.dbApplier.Health(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unavailable", "detail": "db: " + err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// --- Auth handlers (ADR-037) -----------------------------------------------

// userView is the user as seen by clients — never the password hash.
func userView(u store.User) map[string]any {
	return map[string]any{
		"id":    u.ID,
		"email": u.Email,
		"name":  u.Name,
		"role":  u.Role,
	}
}

// login is POST /api/v1/auth/login {"email","password"}. Success sets the
// httpOnly session cookie and returns the user; one 401 for both an
// unknown email and a wrong password (the endpoint must not reveal which).
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.authSvc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "authentication is not enabled on this server"})
		return
	}
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"email\": \"...\", \"password\": \"...\"}"})
		return
	}
	token, u, err := s.authSvc.Login(r.Context(), body.Email, body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
			return
		}
		writeErr(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(auth.SessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"user": userView(u)})
}

// logout is POST /api/v1/auth/logout. It revokes the session behind the
// cookie if there is one and clears the cookie; the response is 200
// whether or not a session existed (logout is idempotent).
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if s.authSvc != nil {
		if c, err := r.Cookie(auth.SessionCookie); err == nil {
			_ = s.authSvc.Logout(r.Context(), c.Value)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

// setupAdmin is POST /api/v1/auth/setup {"name","email","password"} — the
// first-run wizard's endpoint (ADR-037 §6 amendment). It creates the first
// admin only while the users table is empty (409 afterwards, forever), so
// there is never a default credential and never open registration; the UI
// signs the new admin in through the normal login flow.
func (s *Server) setupAdmin(w http.ResponseWriter, r *http.Request) {
	if s.authSvc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "authentication is not enabled on this server"})
		return
	}
	var body struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"name\": ..., \"email\": ..., \"password\": ...}"})
		return
	}
	// The wizard is the admin gate of a fresh deployment: refuse a password
	// that would be trivial to guess before it ever gets hashed.
	var reasons []string
	if strings.TrimSpace(body.Email) == "" {
		reasons = append(reasons, "email is required")
	}
	if len(body.Password) < 8 {
		reasons = append(reasons, "password must be at least 8 characters")
	}
	if len(reasons) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "validation failed", "reasons": reasons})
		return
	}
	u, err := s.authSvc.SetupAdmin(r.Context(), body.Name, body.Email, body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrFirstAdminExists) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "first admin already exists"})
			return
		}
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": userView(u)})
}

// me is GET /api/v1/auth/me — deliberately not behind RequireUser: with
// auth enabled it answers 401 when no session is held, and that 401 body
// carries needs_setup so the first-run wizard advertises itself only while
// the users table is empty. With auth disabled it answers
// auth_enabled:false — the client distinguishes "login required" from
// "auth not enforced" explicitly.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	if s.authSvc == nil {
		writeJSON(w, http.StatusOK, map[string]any{"auth_enabled": false, "user": nil})
		return
	}
	if c, err := r.Cookie(auth.SessionCookie); err == nil && c.Value != "" {
		if u, err := s.authSvc.Authenticate(r.Context(), c.Value); err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"auth_enabled": true, "user": userView(u)})
			return
		}
	}
	// Fail closed: if the store cannot say whether users exist, the wizard
	// does not advertise itself.
	needsSetup := false
	if n, err := s.store.CountUsers(r.Context()); err == nil {
		needsSetup = n == 0
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized", "needs_setup": needsSetup})
}

// applyRecommendation is the write surface: POST
// /api/v1/recommendations/{id}/apply with {"mode","actor"}. All
// guardrail decisions come back as structured reasons. The route picks
// the write engine from the recommendation's resource — class
// (databases, ADR-030) → DB engine, cpu/memory → k8s engine: one write
// surface per kind, decided at the same point the engines enforce it.
//
// With auth enabled (ADR-037) the actor is server-verified: it comes from
// the session user's email and any client-supplied actor is ignored —
// the audit trail records who the server knows acted, never a self-report.
func (s *Server) applyRecommendation(w http.ResponseWriter, r *http.Request) {
	// A fully read-only API (neither write engine configured) answers
	// 503 for the whole surface, whatever the recommendation.
	if s.applier == nil && s.dbApplier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "apply engine not configured (no write identity)"})
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "recommendation id must be an integer"})
		return
	}
	var body struct {
		Mode  string `json:"mode"`
		Actor string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"mode\": \"dry_run|approved|auto\", \"actor\": \"...\"}"})
		return
	}
	rec, err := s.store.GetRecommendation(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "recommendation not found"})
			return
		}
		writeErr(w, err)
		return
	}
	actor := body.Actor
	if s.authSvc != nil {
		u, ok := auth.UserFromContext(r.Context())
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		actor = "api:" + u.Email // client-supplied actor is rejected (ADR-037)
	}
	if rec.Resource == store.ResourceClass {
		s.applyClass(w, r, id, body.Mode, actor)
		return
	}
	s.applyK8s(w, r, id, body.Mode, actor)
}

// applyK8s applies a cpu/memory recommendation through the k8s engine.
func (s *Server) applyK8s(w http.ResponseWriter, r *http.Request, id int64, mode, actor string) {
	if s.applier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "apply engine not configured (no cluster write identity)"})
		return
	}
	res, err := s.applier.Apply(r.Context(), id, mode, actor)
	if err != nil {
		var ge *apply.GuardError
		switch {
		case errors.As(err, &ge):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "apply blocked", "reasons": ge.Reasons})
		case errors.Is(err, apply.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "recommendation not found"})
		default:
			writeErr(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// applyClass applies a database class recommendation through the DB
// engine (ADR-030). A nil engine answers 503, same as the k8s side —
// the API never applies a class through the k8s engine.
func (s *Server) applyClass(w http.ResponseWriter, r *http.Request, id int64, mode, actor string) {
	if s.dbApplier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "db apply engine not configured (no DB write identity)"})
		return
	}
	res, err := s.dbApplier.Apply(r.Context(), id, mode, actor)
	if err != nil {
		var ge *dbapply.GuardError
		switch {
		case errors.As(err, &ge):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "apply blocked", "reasons": ge.Reasons})
		case errors.Is(err, dbapply.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "recommendation not found"})
		default:
			writeErr(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) listApplies(w http.ResponseWriter, r *http.Request) {
	var workloadID *int64
	if v := r.URL.Query().Get("workload_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workload_id must be an integer"})
			return
		}
		workloadID = &id
	}
	events, err := s.store.ListApplyEvents(r.Context(), workloadID, r.URL.Query().Get("result"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applies": events})
}

func (s *Server) listVerificationRuns(w http.ResponseWriter, r *http.Request) {
	var applyEventID *int64
	if v := r.URL.Query().Get("apply_event_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "apply_event_id must be an integer"})
			return
		}
		applyEventID = &id
	}
	runs, err := s.store.ListVerificationRuns(r.Context(), applyEventID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"verification_runs": runs})
}

func (s *Server) listWorkloads(w http.ResponseWriter, r *http.Request) {
	ws, err := s.store.ListWorkloads(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workloads": ws})
}

// --- Teams and ownership (ADR-043) ---------------------------------------

func (s *Server) listTeams(w http.ResponseWriter, r *http.Request) {
	teams, err := s.store.ListTeams(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"teams": teams})
}

func (s *Server) createTeam(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Owner  string `json:"owner"`
		OnCall string `json:"on_call"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"name\",\"owner\",\"on_call\"}"})
		return
	}
	team, reasons := normalizedTeam(body.Name, body.Owner, body.OnCall)
	if len(reasons) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "validation failed", "reasons": reasons})
		return
	}
	created, err := s.store.CreateTeam(r.Context(), team)
	if errors.Is(err, store.ErrConflict) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a team with that name already exists"})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"team": created})
}

func (s *Server) updateTeam(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "team")
	if !ok {
		return
	}
	var body struct {
		Owner  string `json:"owner"`
		OnCall string `json:"on_call"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"owner\",\"on_call\"}"})
		return
	}
	_, reasons := normalizedTeam("team", body.Owner, body.OnCall)
	if len(reasons) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "validation failed", "reasons": reasons})
		return
	}
	updated, err := s.store.UpdateTeam(r.Context(), store.Team{ID: id, Owner: strings.TrimSpace(body.Owner), OnCall: strings.TrimSpace(body.OnCall)})
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "team not found"})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"team": updated})
}

func (s *Server) assignWorkloadTeam(w http.ResponseWriter, r *http.Request) {
	workloadID, ok := pathID(w, r, "workload")
	if !ok {
		return
	}
	var body struct {
		TeamID int64 `json:"team_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"team_id\": <positive integer>}"})
		return
	}
	if body.TeamID < 1 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "team_id must be a positive integer"})
		return
	}
	if _, err := s.store.GetTeam(r.Context(), body.TeamID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "team not found"})
			return
		}
		writeErr(w, err)
		return
	}
	if err := s.store.SetWorkloadTeam(r.Context(), workloadID, &body.TeamID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "workload not found"})
			return
		}
		writeErr(w, err)
		return
	}
	workload, err := s.store.GetWorkload(r.Context(), workloadID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workload": workload})
}

func (s *Server) unassignWorkloadTeam(w http.ResponseWriter, r *http.Request) {
	workloadID, ok := pathID(w, r, "workload")
	if !ok {
		return
	}
	if err := s.store.SetWorkloadTeam(r.Context(), workloadID, nil); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "workload not found"})
			return
		}
		writeErr(w, err)
		return
	}
	workload, err := s.store.GetWorkload(r.Context(), workloadID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workload": workload})
}

func pathID(w http.ResponseWriter, r *http.Request, kind string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": kind + " id must be a positive integer"})
		return 0, false
	}
	return id, true
}

func normalizedTeam(name, owner, onCall string) (store.Team, []string) {
	name, owner, onCall = strings.TrimSpace(name), strings.TrimSpace(owner), strings.TrimSpace(onCall)
	reasons := []string{}
	if name == "" {
		reasons = append(reasons, "name is required")
	}
	if owner == "" {
		reasons = append(reasons, "owner is required")
	}
	if onCall == "" {
		reasons = append(reasons, "on_call is required")
	}
	if len(name) > 80 || len(owner) > 160 || len(onCall) > 240 {
		reasons = append(reasons, "name, owner, and on_call must be reasonably sized")
	}
	slug := teamSlug(name)
	if slug == "" {
		reasons = append(reasons, "name must contain letters or numbers")
	}
	return store.Team{Name: name, Slug: slug, Owner: owner, OnCall: onCall}, reasons
}

func teamSlug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func (s *Server) getWorkload(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workload id must be an integer"})
		return
	}
	wl, err := s.store.GetWorkload(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "workload not found"})
			return
		}
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wl)
}

const (
	recsDefaultLimit = 100
	recsMaxLimit     = 500
)

func (s *Server) listRecommendations(w http.ResponseWriter, r *http.Request) {
	var workloadID *int64
	if v := r.URL.Query().Get("workload_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workload_id must be an integer"})
			return
		}
		workloadID = &id
	}
	// Pagination: ?limit= (default 100, cap 500) and ?offset= (default 0).
	// The total returned by the store lets clients render "N of M" and
	// know when to stop fetching pages.
	limit, offset := recsDefaultLimit, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be a positive integer"})
			return
		}
		if n > recsMaxLimit {
			n = recsMaxLimit
		}
		limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "offset must be a non-negative integer"})
			return
		}
		offset = n
	}
	recs, total, err := s.store.ListRecommendations(r.Context(), workloadID, r.URL.Query().Get("status"), limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Additive M3.5 extension: per-recommendation risk from existing data.
	views := make([]recommendationView, 0, len(recs))
	cache := map[string][]store.Bucket{}
	for _, rec := range recs {
		risk, reasons := s.recRisk(r.Context(), rec, cache)
		views = append(views, recommendationView{
			Recommendation: rec,
			Risk:           risk,
			RiskReasons:    reasons,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"recommendations": views,
		"pagination": map[string]int{
			"limit":  limit,
			"offset": offset,
			"total":  total,
		},
	})
}

func writeErr(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Shutdown helper for servers with graceful stop (kept for cmd/api).
func Shutdown(ctx context.Context, srv *http.Server, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(ctx) }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return errors.New("graceful shutdown timed out")
	}
}
