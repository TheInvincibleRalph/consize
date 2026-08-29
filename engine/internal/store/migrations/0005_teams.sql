-- 0005_teams: workload ownership and on-call directory (ADR-043).
-- A collector never writes team_id, so operator-managed ownership survives
-- each metadata refresh. Deleting a team unassigns workloads rather than
-- deleting infrastructure data or historical recommendations.

CREATE TABLE IF NOT EXISTS teams (
    id         BIGSERIAL PRIMARY KEY,
    slug       TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    owner      TEXT NOT NULL DEFAULT '',
    on_call    TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE workloads
    ADD COLUMN IF NOT EXISTS team_id BIGINT REFERENCES teams(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS workloads_team_id ON workloads(team_id);
