package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"consize/internal/api"
	"consize/internal/apply"
	"consize/internal/auth"
	"consize/internal/pricing"
	"consize/internal/store"
)

// newAuthApplyServer mounts the API with auth enabled (ADR-037) over the
// in-memory store and a live apply engine (recording patcher), so the
// write surface is fully exercisable.
func newAuthApplyServer(t *testing.T) (http.Handler, *store.Memory, *recordingPatcher, *auth.Service) {
	t.Helper()
	st := store.NewMemory()
	authSvc := auth.NewService(st)
	p := &recordingPatcher{}
	applier := apply.NewService(st, p, apply.DefaultConfig())
	h := api.New(st, pricing.Static{P: pricing.DefaultStatic()}, applier, nil, api.Options{Auth: authSvc})
	return h, st, p, authSvc
}

// addUser creates a user through the real bcrypt path.
func addUser(t *testing.T, authSvc *auth.Service, email, password, role string) store.User {
	t.Helper()
	u, err := authSvc.CreateUser(context.Background(), email, "Test "+role, password, role)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// login posts credentials and returns the session cookie value.
func login(t *testing.T, h http.Handler, email, password string) (string, int) {
	t.Helper()
	rec := post(t, h, "/api/v1/auth/login", map[string]string{"email": email, "password": password})
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookie {
			return c.Value, rec.Code
		}
	}
	return "", rec.Code
}

// authed performs a request carrying the session cookie.
func authed(t *testing.T, h http.Handler, method, path string, body any, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cookie})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAuthDisabledSurface pins the disabled build (ADR-037 §3): no
// login surface, me answers auth_enabled:false, reads stay open.
func TestAuthDisabledSurface(t *testing.T) {
	h, _ := newTestServer(t) // no Options → auth disabled
	rec := get(t, h, "/api/v1/auth/me")
	if rec.Code != http.StatusOK {
		t.Fatalf("me: %d", rec.Code)
	}
	var body struct {
		AuthEnabled bool        `json:"auth_enabled"`
		User        *store.User `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.AuthEnabled || body.User != nil {
		t.Fatalf("disabled build must report auth_enabled:false: %s", rec.Body.String())
	}
	if rec := post(t, h, "/api/v1/auth/login", map[string]string{"email": "x", "password": "y"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("login on disabled build: %d, want 503", rec.Code)
	}
	if rec := post(t, h, "/api/v1/auth/setup", map[string]string{"email": "x", "password": "yyyyyyyy"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("setup on disabled build: %d, want 503", rec.Code)
	}
	if rec := get(t, h, "/api/v1/workloads"); rec.Code != http.StatusOK {
		t.Fatalf("read with auth disabled: %d, want 200", rec.Code)
	}
}

// TestLoginFlow covers the happy path plus the single-error rule for
// bad credentials (one 401 whether the email or the password is wrong).
func TestLoginFlow(t *testing.T) {
	h, _, _, authSvc := newAuthApplyServer(t)
	addUser(t, authSvc, "op@example.com", "s3cret", store.RoleOperator)

	if _, code := login(t, h, "op@example.com", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d, want 401", code)
	}
	if _, code := login(t, h, "nobody@example.com", "s3cret"); code != http.StatusUnauthorized {
		t.Fatalf("unknown email: %d, want 401", code)
	}

	token, code := login(t, h, "op@example.com", "s3cret")
	if code != http.StatusOK || token == "" {
		t.Fatalf("login failed: code %d, token %q", code, token)
	}

	// me without a cookie → 401; with the cookie → the user.
	if rec := get(t, h, "/api/v1/auth/me"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("me without cookie: %d, want 401", rec.Code)
	}
	rec := authed(t, h, http.MethodGet, "/api/v1/auth/me", nil, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("me with cookie: %d", rec.Code)
	}
	var me struct {
		User struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.User.Email != "op@example.com" || me.User.Role != store.RoleOperator {
		t.Fatalf("me wrong user: %s", rec.Body.String())
	}
}

// TestAuthHandlerMatrix is the enforcement matrix (M4 AC): reads need a
// session, writes need operator+; viewers read everything and touch
// nothing.
func TestAuthHandlerMatrix(t *testing.T) {
	h, st, p, authSvc := newAuthApplyServer(t)
	addUser(t, authSvc, "view@example.com", "v-pass", store.RoleViewer)
	addUser(t, authSvc, "op@example.com", "o-pass", store.RoleOperator)

	recID := seedPendingRec(t, st, "prod", nil)

	// No cookie: reads 401.
	if rec := get(t, h, "/api/v1/workloads"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-cookie read: %d, want 401", rec.Code)
	}

	viewerTok, code := login(t, h, "view@example.com", "v-pass")
	if code != http.StatusOK {
		t.Fatalf("viewer login: %d", code)
	}
	opTok, code := login(t, h, "op@example.com", "o-pass")
	if code != http.StatusOK {
		t.Fatalf("operator login: %d", code)
	}

	// Viewer reads are fine.
	if rec := authed(t, h, http.MethodGet, "/api/v1/workloads", nil, viewerTok); rec.Code != http.StatusOK {
		t.Fatalf("viewer read: %d, want 200", rec.Code)
	}
	if rec := authed(t, h, http.MethodGet, "/api/v1/savings", nil, viewerTok); rec.Code != http.StatusOK {
		t.Fatalf("viewer savings: %d, want 200", rec.Code)
	}

	// Viewer apply → 403 with the required role in the body.
	rec := authed(t, h, http.MethodPost, "/api/v1/recommendations/"+itoa(recID)+"/apply",
		map[string]string{"mode": "dry_run"}, viewerTok)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer apply: %d, want 403", rec.Code)
	}
	var forbidden struct {
		Error        string `json:"error"`
		RoleRequired string `json:"role_required"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &forbidden); err != nil {
		t.Fatal(err)
	}
	if forbidden.Error != "forbidden" || forbidden.RoleRequired != store.RoleOperator {
		t.Fatalf("forbidden body: %s", rec.Body.String())
	}

	// No cookie on apply → 401.
	if rec := authed(t, h, http.MethodPost, "/api/v1/recommendations/"+itoa(recID)+"/apply",
		map[string]string{"mode": "dry_run"}, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-cookie apply: %d, want 401", rec.Code)
	}

	// Operator dry-run → 200, nothing patched.
	if rec := authed(t, h, http.MethodPost, "/api/v1/recommendations/"+itoa(recID)+"/apply",
		map[string]string{"mode": "dry_run"}, opTok); rec.Code != http.StatusOK {
		t.Fatalf("operator dry run: %d %s", rec.Code, rec.Body.String())
	}
	if p.patches != 0 {
		t.Fatalf("dry run patched: %d patches", p.patches)
	}
}

// TestTeamOwnershipAuthorization pins ADR-043's configuration boundary:
// every signed-in user may see ownership, while only admins can create teams
// or assign workloads to them. It also verifies that the API exposes the
// resulting owner and on-call contact with the workload.
func TestTeamOwnershipAuthorization(t *testing.T) {
	h, st, _, authSvc := newAuthApplyServer(t)
	addUser(t, authSvc, "view@example.com", "v-pass", store.RoleViewer)
	addUser(t, authSvc, "op@example.com", "o-pass", store.RoleOperator)
	addUser(t, authSvc, "admin@example.com", "a-pass", store.RoleAdmin)
	wid, err := st.UpsertWorkload(context.Background(), store.Workload{Name: "checkout", Namespace: "prod", Source: "k8s"})
	if err != nil {
		t.Fatal(err)
	}

	viewer, _ := login(t, h, "view@example.com", "v-pass")
	op, _ := login(t, h, "op@example.com", "o-pass")
	admin, _ := login(t, h, "admin@example.com", "a-pass")

	if rec := get(t, h, "/api/v1/teams"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-cookie teams read: %d, want 401", rec.Code)
	}
	if rec := authed(t, h, http.MethodGet, "/api/v1/teams", nil, viewer); rec.Code != http.StatusOK {
		t.Fatalf("viewer teams read: %d, want 200", rec.Code)
	}

	teamBody := map[string]string{"name": "Payments Platform", "owner": "Ada", "on_call": "#payments-oncall"}
	if rec := authed(t, h, http.MethodPost, "/api/v1/teams", teamBody, op); rec.Code != http.StatusForbidden {
		t.Fatalf("operator team create: %d, want 403", rec.Code)
	}
	created := authed(t, h, http.MethodPost, "/api/v1/teams", teamBody, admin)
	if created.Code != http.StatusCreated {
		t.Fatalf("admin team create: %d %s", created.Code, created.Body.String())
	}
	var out struct {
		Team store.Team `json:"team"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Team.ID == 0 || out.Team.Slug != "payments-platform" {
		t.Fatalf("created team: %+v", out.Team)
	}

	if rec := authed(t, h, http.MethodPut, "/api/v1/workloads/"+itoa(wid)+"/team", map[string]int64{"team_id": out.Team.ID}, op); rec.Code != http.StatusForbidden {
		t.Fatalf("operator assignment: %d, want 403", rec.Code)
	}
	assigned := authed(t, h, http.MethodPut, "/api/v1/workloads/"+itoa(wid)+"/team", map[string]int64{"team_id": out.Team.ID}, admin)
	if assigned.Code != http.StatusOK {
		t.Fatalf("admin assignment: %d %s", assigned.Code, assigned.Body.String())
	}
	read := authed(t, h, http.MethodGet, "/api/v1/workloads/"+itoa(wid), nil, viewer)
	if read.Code != http.StatusOK || !bytes.Contains(read.Body.Bytes(), []byte(`"TeamName":"Payments Platform"`)) ||
		!bytes.Contains(read.Body.Bytes(), []byte(`"TeamOnCall":"#payments-oncall"`)) {
		t.Fatalf("owned workload response: %d %s", read.Code, read.Body.String())
	}

	updated := authed(t, h, http.MethodPatch, "/api/v1/teams/"+itoa(out.Team.ID), map[string]string{
		"owner": "Grace", "on_call": "payments@example.com",
	}, admin)
	if updated.Code != http.StatusOK {
		t.Fatalf("team update: %d %s", updated.Code, updated.Body.String())
	}
	read = authed(t, h, http.MethodGet, "/api/v1/workloads/"+itoa(wid), nil, viewer)
	if !bytes.Contains(read.Body.Bytes(), []byte(`"TeamOnCall":"payments@example.com"`)) {
		t.Fatalf("updated on-call missing: %s", read.Body.String())
	}
}

func TestAlertingConfigAuthorizationAndValidation(t *testing.T) {
	h, _, _, authSvc := newAuthApplyServer(t)
	addUser(t, authSvc, "view@example.com", "v-pass", store.RoleViewer)
	addUser(t, authSvc, "op@example.com", "o-pass", store.RoleOperator)
	addUser(t, authSvc, "admin@example.com", "a-pass", store.RoleAdmin)

	viewer, _ := login(t, h, "view@example.com", "v-pass")
	op, _ := login(t, h, "op@example.com", "o-pass")
	admin, _ := login(t, h, "admin@example.com", "a-pass")

	if rec := authed(t, h, http.MethodGet, "/api/v1/alerting/config", nil, viewer); rec.Code != http.StatusOK {
		t.Fatalf("viewer alert config read: %d, want 200", rec.Code)
	}

	valid := map[string]any{
		"default_contact_point": "ops-slack",
		"contact_points": []map[string]any{{
			"name": "ops-slack",
			"integrations": []map[string]any{{
				"type":        "slack",
				"webhook_env": "CONSIZE_SLACK_WEBHOOK",
				"channel":     "#platform-oncall",
				"mention":     "<!subteam^S123>",
			}},
		}},
		"notification_policies": []map[string]any{{
			"name":          "critical",
			"match":         map[string]string{"severity": "critical"},
			"contact_point": "ops-slack",
		}},
	}
	if rec := authed(t, h, http.MethodPut, "/api/v1/alerting/config", valid, op); rec.Code != http.StatusForbidden {
		t.Fatalf("operator alert config write: %d, want 403", rec.Code)
	}
	saved := authed(t, h, http.MethodPut, "/api/v1/alerting/config", valid, admin)
	if saved.Code != http.StatusOK {
		t.Fatalf("admin alert config write: %d %s", saved.Code, saved.Body.String())
	}
	var out struct {
		Source string `json:"source"`
		Config struct {
			DefaultContactPoint string `json:"default_contact_point"`
		} `json:"config"`
	}
	if err := json.Unmarshal(saved.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Source != "store" || out.Config.DefaultContactPoint != "ops-slack" {
		t.Fatalf("saved response: %s", saved.Body.String())
	}

	read := authed(t, h, http.MethodGet, "/api/v1/alerting/config", nil, viewer)
	if read.Code != http.StatusOK {
		t.Fatalf("read saved alert config: %d", read.Code)
	}
	if !bytes.Contains(read.Body.Bytes(), []byte(`"source":"store"`)) {
		t.Fatalf("read did not come from store: %s", read.Body.String())
	}

	withSecret := map[string]any{
		"default_contact_point": "bad",
		"contact_points": []map[string]any{{
				"name": "bad",
				"integrations": []map[string]any{{
					"type":        "slack",
					"webhook_url": "https://hooks.slack.com" + "/services" + "/raw-secret",
				}},
			}},
		"notification_policies": []map[string]any{{
			"name":          "bad",
			"contact_point": "bad",
		}},
	}
	if rec := authed(t, h, http.MethodPut, "/api/v1/alerting/config", withSecret, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("raw webhook_url write: %d, want 400", rec.Code)
	}
}

func TestReportConfigAuthorizationAndDownload(t *testing.T) {
	h, _, _, authSvc := newAuthApplyServer(t)
	addUser(t, authSvc, "view@example.com", "v-pass", store.RoleViewer)
	addUser(t, authSvc, "op@example.com", "o-pass", store.RoleOperator)
	addUser(t, authSvc, "admin@example.com", "a-pass", store.RoleAdmin)

	viewer, _ := login(t, h, "view@example.com", "v-pass")
	op, _ := login(t, h, "op@example.com", "o-pass")
	admin, _ := login(t, h, "admin@example.com", "a-pass")

	if rec := authed(t, h, http.MethodGet, "/api/v1/reports/config", nil, viewer); rec.Code != http.StatusOK {
		t.Fatalf("viewer report config read: %d, want 200", rec.Code)
	}
	if rec := authed(t, h, http.MethodGet, "/api/v1/reports/savings?range=14d", nil, viewer); rec.Code != http.StatusOK {
		t.Fatalf("viewer report read: %d, want 200", rec.Code)
	}
	pdf := authed(t, h, http.MethodGet, "/api/v1/reports/savings?range=7d&format=pdf", nil, viewer)
	if pdf.Code != http.StatusOK || pdf.Header().Get("Content-Type") != "application/pdf" || !bytes.HasPrefix(pdf.Body.Bytes(), []byte("%PDF-1.4")) {
		t.Fatalf("viewer pdf: code=%d content-type=%q body=%q", pdf.Code, pdf.Header().Get("Content-Type"), pdf.Body.String())
	}

	cfg := map[string]any{"enabled": true, "range_days": 14}
	if rec := authed(t, h, http.MethodPut, "/api/v1/reports/config", cfg, op); rec.Code != http.StatusForbidden {
		t.Fatalf("operator report config write: %d, want 403", rec.Code)
	}
	if rec := authed(t, h, http.MethodPut, "/api/v1/reports/config", cfg, admin); rec.Code != http.StatusOK {
		t.Fatalf("admin report config write: %d %s", rec.Code, rec.Body.String())
	}
	if rec := authed(t, h, http.MethodPut, "/api/v1/reports/config", map[string]any{"range_days": 90}, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad range: %d, want 400", rec.Code)
	}
	if rec := authed(t, h, http.MethodPost, "/api/v1/reports/send", map[string]any{"range_days": 7}, op); rec.Code != http.StatusForbidden {
		t.Fatalf("operator report send: %d, want 403", rec.Code)
	}
}

func TestCostOpportunityAuthorization(t *testing.T) {
	h, _, _, authSvc := newAuthApplyServer(t)
	addUser(t, authSvc, "view@example.com", "v-pass", store.RoleViewer)
	addUser(t, authSvc, "op@example.com", "o-pass", store.RoleOperator)

	viewer, _ := login(t, h, "view@example.com", "v-pass")
	op, _ := login(t, h, "op@example.com", "o-pass")

	if rec := authed(t, h, http.MethodGet, "/api/v1/cost-opportunities", nil, viewer); rec.Code != http.StatusOK {
		t.Fatalf("viewer opportunity read: %d, want 200", rec.Code)
	}
	if rec := authed(t, h, http.MethodPost, "/api/v1/cost-opportunities/scan", nil, viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer scan: %d, want 403", rec.Code)
	}
	scan := authed(t, h, http.MethodPost, "/api/v1/cost-opportunities/scan", nil, op)
	if scan.Code != http.StatusOK {
		t.Fatalf("operator scan: %d %s", scan.Code, scan.Body.String())
	}
	var body struct {
		Opportunities []struct {
			ID int64 `json:"ID"`
		} `json:"opportunities"`
	}
	if err := json.Unmarshal(scan.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Opportunities) == 0 {
		t.Fatal("operator scan returned no opportunities")
	}
	emptyBody := map[string]any{}
	if rec := authed(t, h, http.MethodPost, "/api/v1/cost-opportunities/"+itoa(body.Opportunities[0].ID)+"/iac-pr", emptyBody, viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer pr plan: %d, want 403", rec.Code)
	}
	if rec := authed(t, h, http.MethodPost, "/api/v1/cost-opportunities/"+itoa(body.Opportunities[0].ID)+"/iac-pr", emptyBody, op); rec.Code != http.StatusOK {
		t.Fatalf("operator pr plan: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRecommendationIaCPullRequestAuthorization(t *testing.T) {
	h, st, _, authSvc := newAuthApplyServer(t)
	recID := seedPendingRec(t, st, "apps", nil)
	addUser(t, authSvc, "view@example.com", "v-pass", store.RoleViewer)
	addUser(t, authSvc, "op@example.com", "o-pass", store.RoleOperator)

	viewer, _ := login(t, h, "view@example.com", "v-pass")
	op, _ := login(t, h, "op@example.com", "o-pass")

	emptyBody := map[string]any{}
	if rec := authed(t, h, http.MethodPost, "/api/v1/recommendations/"+itoa(recID)+"/iac-pr", emptyBody, viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer recommendation pr plan: %d, want 403", rec.Code)
	}
	if rec := authed(t, h, http.MethodPost, "/api/v1/recommendations/"+itoa(recID)+"/iac-pr", emptyBody, op); rec.Code != http.StatusOK {
		t.Fatalf("operator recommendation pr plan: %d %s", rec.Code, rec.Body.String())
	}
}

// TestActorIsServerVerified is the ADR-037 core contract: a client-supplied
// actor is ignored; the audit trail records "api:<email>" from the session.
func TestActorIsServerVerified(t *testing.T) {
	h, st, _, authSvc := newAuthApplyServer(t)
	recID := seedPendingRec(t, st, "prod", nil)
	addUser(t, authSvc, "op@example.com", "o-pass", store.RoleOperator)

	token, code := login(t, h, "op@example.com", "o-pass")
	if code != http.StatusOK {
		t.Fatalf("login: %d", code)
	}

	// The client tries to forge the actor.
	rec := authed(t, h, http.MethodPost, "/api/v1/recommendations/"+itoa(recID)+"/apply",
		map[string]string{"mode": "dry_run", "actor": "attacker"}, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}

	// The trail must name the session user, never "attacker".
	events := authed(t, h, http.MethodGet, "/api/v1/applies", nil, token)
	if !bytes.Contains(events.Body.Bytes(), []byte(`"Actor":"api:op@example.com"`)) {
		t.Fatalf("actor not server-verified: %s", events.Body.String())
	}
	if bytes.Contains(events.Body.Bytes(), []byte(`attacker`)) {
		t.Fatalf("client-supplied actor leaked into the trail: %s", events.Body.String())
	}
}

// TestExpiredSessionAndLogout: an expired session and a revoked session
// both fail auth; logout clears the cookie.
func TestExpiredSessionAndLogout(t *testing.T) {
	h, st, _, authSvc := newAuthApplyServer(t)
	addUser(t, authSvc, "view@example.com", "v-pass", store.RoleViewer)

	// Expired session: stored directly with a past expiry, the cookie
	// value is the token whose SHA-256 the store holds.
	raw := "expired-token"
	sum := sha256.Sum256([]byte(raw))
	if err := st.CreateSession(context.Background(), store.Session{
		TokenHash: hex.EncodeToString(sum[:]),
		UserID:    1, // the first user created
		ExpiresAt: time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if rec := authed(t, h, http.MethodGet, "/api/v1/workloads", nil, raw); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired session: %d, want 401", rec.Code)
	}

	// Logout revokes the live session — it revokes by reading the
	// request's cookie, so the call must carry it.
	token, code := login(t, h, "view@example.com", "v-pass")
	if code != http.StatusOK {
		t.Fatalf("login: %d", code)
	}
	if rec := authed(t, h, http.MethodPost, "/api/v1/auth/logout", nil, token); rec.Code != http.StatusOK {
		t.Fatalf("logout: %d", rec.Code)
	}
	if rec := authed(t, h, http.MethodGet, "/api/v1/auth/me", nil, token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d, want 401", rec.Code)
	}
	if rec := authed(t, h, http.MethodGet, "/api/v1/workloads", nil, token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("read after logout: %d, want 401", rec.Code)
	}
}

// TestFirstAdminSetup is the first-run wizard contract (ADR-037 §6
// amendment): me() advertises needs_setup only while the users table is
// empty, setup creates the first admin exactly once (409 forever after),
// and the created admin signs in through the normal login flow.
func TestFirstAdminSetup(t *testing.T) {
	h, _, _, _ := newAuthApplyServer(t)

	// No session, no users: 401 with the setup advertisement.
	rec := get(t, h, "/api/v1/auth/me")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("me on empty store: %d, want 401", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"needs_setup":true`)) {
		t.Fatalf("me on empty store must advertise needs_setup: %s", rec.Body.String())
	}

	// A weak password is refused before it ever gets hashed.
	if rec := post(t, h, "/api/v1/auth/setup", map[string]string{"email": "admin@example.com", "password": "short"}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("weak password: %d, want 422", rec.Code)
	}
	if rec := get(t, h, "/api/v1/auth/me"); !bytes.Contains(rec.Body.Bytes(), []byte(`"needs_setup":true`)) {
		t.Fatalf("failed setup must not close the wizard: %s", rec.Body.String())
	}

	// First call creates the admin.
	rec = post(t, h, "/api/v1/auth/setup", map[string]string{"name": "Root", "email": "Admin@Example.com", "password": "longenough-pw"})
	if rec.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		User struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.User.Email != "admin@example.com" || created.User.Role != store.RoleAdmin {
		t.Fatalf("setup user wrong: %s", rec.Body.String())
	}

	// Users exist now: setup → 409, and me() stops advertising.
	if rec := post(t, h, "/api/v1/auth/setup", map[string]string{"email": "other@example.com", "password": "longenough-pw"}); rec.Code != http.StatusConflict {
		t.Fatalf("second setup: %d, want 409", rec.Code)
	}
	rec = get(t, h, "/api/v1/auth/me")
	if bytes.Contains(rec.Body.Bytes(), []byte(`"needs_setup":true`)) {
		t.Fatalf("me must not advertise setup once users exist: %s", rec.Body.String())
	}

	// The created admin signs in through the normal flow.
	token, code := login(t, h, "admin@example.com", "longenough-pw")
	if code != http.StatusOK || token == "" {
		t.Fatalf("admin login after setup: %d", code)
	}
	if rec := authed(t, h, http.MethodGet, "/api/v1/auth/me", nil, token); rec.Code != http.StatusOK {
		t.Fatalf("me with the setup admin's session: %d", rec.Code)
	}
}
