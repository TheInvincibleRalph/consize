-- 0001_init: core tables for Consize M1.
-- The audit trail (apply_events, verification_runs) lands in M2.

CREATE TABLE IF NOT EXISTS workloads (
    id                BIGSERIAL PRIMARY KEY,
    name              TEXT NOT NULL,
    namespace         TEXT NOT NULL DEFAULT 'default',
    kind              TEXT NOT NULL DEFAULT 'deployment',
    labels            JSONB NOT NULL DEFAULT '{}',
    request_cpu_milli BIGINT NOT NULL DEFAULT 0,
    limit_cpu_milli   BIGINT NOT NULL DEFAULT 0,
    request_mem_bytes BIGINT NOT NULL DEFAULT 0,
    limit_mem_bytes   BIGINT NOT NULL DEFAULT 0,
    source            TEXT NOT NULL DEFAULT 'k8s',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source, name, namespace)
);

CREATE TABLE IF NOT EXISTS usage_buckets (
    id           BIGSERIAL PRIMARY KEY,
    workload_id  BIGINT NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
    metric       TEXT NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    p50          DOUBLE PRECISION NOT NULL DEFAULT 0,
    p95          DOUBLE PRECISION NOT NULL DEFAULT 0,
    p99          DOUBLE PRECISION NOT NULL DEFAULT 0,
    max_value    DOUBLE PRECISION NOT NULL DEFAULT 0,
    sample_count INT NOT NULL DEFAULT 0,
    UNIQUE (workload_id, metric, window_start)
);

CREATE INDEX IF NOT EXISTS usage_buckets_lookup
    ON usage_buckets (workload_id, metric, window_start);

CREATE TABLE IF NOT EXISTS recommendations (
    id              BIGSERIAL PRIMARY KEY,
    workload_id     BIGINT NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
    resource        TEXT NOT NULL,
    current_value   BIGINT NOT NULL,
    proposed_value  BIGINT NOT NULL,
    savings_monthly NUMERIC(12, 2) NOT NULL DEFAULT 0,
    confidence      DOUBLE PRECISION NOT NULL DEFAULT 0,
    policy_version  TEXT NOT NULL DEFAULT 'v1',
    status          TEXT NOT NULL DEFAULT 'pending',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS recommendations_lookup
    ON recommendations (workload_id, status);
