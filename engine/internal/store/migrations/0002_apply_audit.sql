-- 0002_apply_audit: the safety-engine audit trail (M2).
-- apply_events is INSERT-only by design (ADR-008): outcomes are new
-- rows, never edits. In-flight state is derived (applied event without
-- a verification run), so crashes leave a retryable trail, not a lie.

-- Recommendations now carry the limit pair the policy computed, so the
-- apply engine can patch request + limit together.
ALTER TABLE recommendations
    ADD COLUMN IF NOT EXISTS current_limit  BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS proposed_limit BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS apply_events (
    id                BIGSERIAL PRIMARY KEY,
    recommendation_id BIGINT NOT NULL REFERENCES recommendations(id) ON DELETE CASCADE,
    workload_id       BIGINT NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
    actor             TEXT NOT NULL,
    mode              TEXT NOT NULL,      -- dry_run | approved | auto
    result            TEXT NOT NULL,      -- planned | applied | reverted
    diff              JSONB NOT NULL DEFAULT '{}',
    step_number       INT NOT NULL DEFAULT 1,
    total_steps       INT NOT NULL DEFAULT 1,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS apply_events_lookup
    ON apply_events (workload_id, created_at DESC);
CREATE INDEX IF NOT EXISTS apply_events_result
    ON apply_events (result);

CREATE TABLE IF NOT EXISTS verification_runs (
    id              BIGSERIAL PRIMARY KEY,
    apply_event_id  BIGINT NOT NULL UNIQUE REFERENCES apply_events(id) ON DELETE CASCADE,
    baseline_start  TIMESTAMPTZ NOT NULL,
    baseline_end    TIMESTAMPTZ NOT NULL,
    post_start      TIMESTAMPTZ NOT NULL,
    post_end        TIMESTAMPTZ NOT NULL,
    verdict         TEXT NOT NULL,        -- passed | failed | inconclusive
    slis            JSONB NOT NULL DEFAULT '{}',
    thresholds      JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
