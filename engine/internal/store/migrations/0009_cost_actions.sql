-- 0009_cost_actions: insert-only audit trail for cloud-waste direct cleanup.
--
-- Cost opportunities are mutable findings: a scanner refresh updates last_seen,
-- status changes from open to resolved, and stale records may disappear from the
-- active list. Provider cleanup itself still needs immutable evidence, so every
-- direct action records requested -> applied/dry_run/failed rows here.

CREATE TABLE IF NOT EXISTS cost_actions (
    id             BIGSERIAL PRIMARY KEY,
    opportunity_id BIGINT NOT NULL REFERENCES cost_opportunities(id) ON DELETE CASCADE,
    actor          TEXT NOT NULL DEFAULT '',
    mode           TEXT NOT NULL DEFAULT 'dry_run',
    result         TEXT NOT NULL DEFAULT 'requested',
    message        TEXT NOT NULL DEFAULT '',
    evidence       JSONB NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS cost_actions_lookup
    ON cost_actions (opportunity_id, created_at DESC);

CREATE INDEX IF NOT EXISTS cost_actions_result
    ON cost_actions (result, created_at DESC);
