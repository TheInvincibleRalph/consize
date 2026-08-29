-- 0004_auth: enterprise authentication (ADR-037).
-- Local users with roles (viewer | operator | admin) and revocable,
-- Postgres-backed sessions. Session tokens are 32 random bytes returned
-- to the client exactly once; only their SHA-256 hash is stored, so the
-- table is not a credential store and a leak cannot be replayed. Roles
-- map to docs/security.md's consize:view / consize:operator /
-- consize:admin.

CREATE TABLE IF NOT EXISTS users (
    id            BIGSERIAL PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'viewer' CHECK (role IN ('viewer', 'operator', 'admin')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_sessions_user    ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);
