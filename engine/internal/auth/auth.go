// Package auth implements ADR-037: local-user authentication with
// server-verified, revocable sessions and role-based authorization.
//
// The API only depends on the Authenticator seam; Service (LocalUsers)
// implements it today and an OIDC provider (docs/security.md §2) can
// implement it later without touching the API. Sessions are Postgres-backed:
// the raw token (32 random bytes) reaches the client once, only its SHA-256
// hash is stored, and expiry/revocation are store semantics (ADR-037 §2).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"consize/internal/store"
)

// Sentinels.
var (
	// ErrInvalidCredentials covers both an unknown email and a wrong
	// password — one error, so the endpoint does not reveal which.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrNoSession means the token is absent, unknown, expired, or revoked.
	ErrNoSession = errors.New("no valid session")
	// ErrInvalidRole means CreateUser was given a role outside
	// viewer|operator|admin.
	ErrInvalidRole = errors.New("invalid role")
	// ErrFirstAdminExists means the users table is not empty — the
	// first-run setup window is closed (ADR-037 §6 amendment).
	ErrFirstAdminExists = errors.New("first admin already exists")
)

// SessionCookie is the httpOnly session cookie name (api writes it, the
// middleware reads it).
const SessionCookie = "consize_session"

// SessionTTL is the lifetime of one login.
const SessionTTL = 7 * 24 * time.Hour

// tokenBytes is the entropy of a session token.
const tokenBytes = 32

// Authenticator is the identity seam. The middleware and the API resolve
// every request through it; Service satisfies it today (local users), an
// OIDC provider slots in as a second implementation.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (store.User, error)
}

// Service is the local-users identity provider.
type Service struct {
	st     store.Store
	now    func() time.Time
	ttl    time.Duration
	hasher func(password string) (string, error)
}

// NewService returns a local-users authenticator. Sessions last SessionTTL.
func NewService(st store.Store) *Service {
	return &Service{
		st:  st,
		now: time.Now,
		ttl: SessionTTL,
		hasher: func(password string) (string, error) {
			b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
			return string(b), err
		},
	}
}

// Login verifies email/password and returns a fresh session token plus the
// user. The token is returned to the client exactly once; the store keeps
// only its hash, so a database leak cannot be replayed.
func (s *Service) Login(ctx context.Context, email, password string) (string, store.User, error) {
	u, err := s.st.GetUserByEmail(ctx, email)
	if err != nil {
		return "", store.User{}, ErrInvalidCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return "", store.User{}, ErrInvalidCredentials
	}

	token, err := newToken()
	if err != nil {
		return "", store.User{}, err
	}
	now := s.now().UTC()
	if err := s.st.CreateSession(ctx, store.Session{
		TokenHash: hashToken(token),
		UserID:    u.ID,
		ExpiresAt: now.Add(s.ttl),
	}); err != nil {
		return "", store.User{}, err
	}
	return token, u, nil
}

// Authenticate resolves a session token to its user. Absent, expired, or
// revoked tokens all surface as ErrNoSession.
func (s *Service) Authenticate(ctx context.Context, token string) (store.User, error) {
	sess, err := s.st.GetSessionByTokenHash(ctx, hashToken(token))
	if err != nil {
		return store.User{}, ErrNoSession
	}
	u, err := s.st.GetUser(ctx, sess.UserID)
	if err != nil {
		return store.User{}, ErrNoSession
	}
	return u, nil
}

// Logout revokes the session behind token (if any).
func (s *Service) Logout(ctx context.Context, token string) error {
	return s.st.DeleteSession(ctx, hashToken(token))
}

// CreateUser adds a local user with a bcrypt-hashed password. Roles are
// validated here, the single enforcement point (the DB CHECK mirrors it).
func (s *Service) CreateUser(ctx context.Context, email, name, password, role string) (store.User, error) {
	switch role {
	case store.RoleViewer, store.RoleOperator, store.RoleAdmin:
	default:
		return store.User{}, ErrInvalidRole
	}
	hash, err := s.hasher(password)
	if err != nil {
		return store.User{}, err
	}
	return s.st.CreateUser(ctx, store.User{
		Email:        strings.ToLower(email),
		Name:         name,
		PasswordHash: hash,
		Role:         role,
	})
}

// CreateBootstrapAdmin honors CONSIZE_BOOTSTRAP_ADMIN: it creates the
// first admin only when the users table is empty, and reports whether it
// did. On any later start the variable is ignored — the bootstrap admin is
// a one-time, out-of-band fact (ADR-037 §4).
func (s *Service) CreateBootstrapAdmin(ctx context.Context, email, password string) (bool, error) {
	n, err := s.st.CountUsers(ctx)
	if err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	if _, err := s.CreateUser(ctx, email, "", password, store.RoleAdmin); err != nil {
		return false, err
	}
	return true, nil
}

// SetupAdmin is the first-run wizard's server half (ADR-037 §6 amendment):
// it creates the first admin while the users table is empty and reports
// ErrFirstAdminExists afterwards, so the API can answer 409. The wizard
// replaces the bootstrap env var for ad-hoc deployments; CreateBootstrapAdmin
// stays for scripted ones — both share the same one-admin-ever gate, so the
// first to run wins and there is never a second, unowned account.
func (s *Service) SetupAdmin(ctx context.Context, name, email, password string) (store.User, error) {
	n, err := s.st.CountUsers(ctx)
	if err != nil {
		return store.User{}, err
	}
	if n > 0 {
		return store.User{}, ErrFirstAdminExists
	}
	return s.CreateUser(ctx, email, name, password, store.RoleAdmin)
}

// --- HTTP middleware -------------------------------------------------------

type ctxKey int

const userCtxKey ctxKey = 0

// UserFromContext returns the user RequireUser placed in the context.
func UserFromContext(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userCtxKey).(store.User)
	return u, ok
}

// RequireUser resolves the session cookie and puts the user in the context.
// A nil Authenticator (auth disabled) passes everything through, so the
// disabled build behaves exactly as before ADR-037.
func RequireUser(a Authenticator) func(http.Handler) http.Handler {
	if a == nil {
		return func(h http.Handler) http.Handler { return h }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(SessionCookie)
			if err != nil || c.Value == "" {
				writeUnauthorized(w)
				return
			}
			u, err := a.Authenticate(r.Context(), c.Value)
			if err != nil {
				writeUnauthorized(w)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, u)))
		})
	}
}

// RequireRole enforces role on the user RequireUser put in the context;
// compose it after RequireUser. A nil Authenticator passes through (the
// disabled build has no users and therefore no roles).
func RequireRole(a Authenticator, role string) func(http.Handler) http.Handler {
	if a == nil {
		return func(h http.Handler) http.Handler { return h }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := UserFromContext(r.Context())
			if !ok {
				writeUnauthorized(w)
				return
			}
			if !roleAtLeast(u.Role, role) {
				writeJSON(w, http.StatusForbidden, map[string]any{
					"error":         "forbidden",
					"role_required": role,
					"your_role":     u.Role,
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// roleAtLeast orders viewer < operator < admin and reports whether have
// satisfies want.
func roleAtLeast(have, want string) bool {
	rank := map[string]int{store.RoleViewer: 1, store.RoleOperator: 2, store.RoleAdmin: 3}
	return rank[have] >= rank[want]
}

func writeUnauthorized(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Handlers in this package only write map literals; encoding failure is
	// not a realistic path, so it is deliberately ignored here.
	_ = json.NewEncoder(w).Encode(body)
}

// newToken returns tokenBytes random bytes, hex-encoded.
func newToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashToken is the SHA-256 of a session token — what the store persists.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
