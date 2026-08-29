package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"consize/internal/store"
)

// TestAuthUsers covers the user surface (ADR-037): idempotent creation on
// email, case-insensitive lookup, and the CountUsers bootstrap gate.
func TestAuthUsers(t *testing.T) {
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			// Empty bootstrap gate.
			n, err := st.CountUsers(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("CountUsers on fresh store = %d, want 0", n)
			}

			u := store.User{
				Email:        "Ada@Example.com", // casing must not matter
				Name:         "Ada Lovelace",
				PasswordHash: "bcrypt-hash",
				Role:         store.RoleAdmin,
			}
			created, err := st.CreateUser(ctx, u)
			if err != nil {
				t.Fatal(err)
			}
			if created.ID == 0 {
				t.Fatal("CreateUser returned zero ID")
			}
			if created.Email != "ada@example.com" {
				t.Fatalf("CreateUser did not normalize email: %q", created.Email)
			}

			// Idempotent on email: same email returns the existing row.
			again, err := st.CreateUser(ctx, store.User{
				Email:        "ADA@example.com",
				Name:         "Changed",
				PasswordHash: "other",
				Role:         store.RoleViewer,
			})
			if err != nil {
				t.Fatal(err)
			}
			if again.ID != created.ID {
				t.Fatalf("CreateUser duplicated email: %d → %d", created.ID, again.ID)
			}

			// Lookups are case-insensitive and find the original row.
			byEmail, err := st.GetUserByEmail(ctx, "ada@example.com")
			if err != nil {
				t.Fatal(err)
			}
			if byEmail.ID != created.ID || byEmail.Name != "Ada Lovelace" || byEmail.Role != store.RoleAdmin {
				t.Fatalf("GetUserByEmail wrong row: %+v", byEmail)
			}
			byID, err := st.GetUser(ctx, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if byID.Email != "ada@example.com" {
				t.Fatalf("GetUser wrong email: %+v", byID)
			}

			// CountUsers reflects the single row.
			n, err = st.CountUsers(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("CountUsers = %d, want 1", n)
			}

			// Unknown email and ID → ErrNotFound.
			if _, err := st.GetUserByEmail(ctx, "nobody@example.com"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetUserByEmail unknown: err = %v, want ErrNotFound", err)
			}
			if _, err := st.GetUser(ctx, 99999); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetUser unknown: err = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestAuthSessions covers the session surface: round-trip, expiry deletion,
// and logout revocation.
func TestAuthSessions(t *testing.T) {
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			u, err := st.CreateUser(ctx, store.User{
				Email:        "op@example.com",
				PasswordHash: "bcrypt-hash",
				Role:         store.RoleOperator,
			})
			if err != nil {
				t.Fatal(err)
			}

			// Round-trip: a live session is found and names its user.
			if err := st.CreateSession(ctx, store.Session{
				TokenHash: "hash-1",
				UserID:    u.ID,
				ExpiresAt: time.Now().UTC().Add(7 * 24 * time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			s, err := st.GetSessionByTokenHash(ctx, "hash-1")
			if err != nil {
				t.Fatal(err)
			}
			if s.UserID != u.ID || !s.ExpiresAt.After(time.Now().UTC()) {
				t.Fatalf("session round-trip wrong: %+v", s)
			}

			// Unknown hash → ErrNotFound.
			if _, err := st.GetSessionByTokenHash(ctx, "hash-nope"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown session: err = %v, want ErrNotFound", err)
			}

			// Expired session is gone: lookup deletes it and returns
			// ErrNotFound (revocable sessions, ADR-037).
			if err := st.CreateSession(ctx, store.Session{
				TokenHash: "hash-expired",
				UserID:    u.ID,
				ExpiresAt: time.Now().UTC().Add(-time.Minute),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.GetSessionByTokenHash(ctx, "hash-expired"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("expired session: err = %v, want ErrNotFound", err)
			}
			if _, err := st.GetSessionByTokenHash(ctx, "hash-expired"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("expired session survived lookup: err = %v, want ErrNotFound", err)
			}

			// Logout revokes: second lookup fails.
			if err := st.DeleteSession(ctx, "hash-1"); err != nil {
				t.Fatal(err)
			}
			if _, err := st.GetSessionByTokenHash(ctx, "hash-1"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("revoked session still valid: err = %v, want ErrNotFound", err)
			}
		})
	}
}
