-- 0007_cost_opportunities: unmanaged cloud-cost findings and IaC PR audit.

CREATE TABLE IF NOT EXISTS cost_opportunities (
    id              BIGSERIAL PRIMARY KEY,
    provider        TEXT NOT NULL,
    account         TEXT NOT NULL DEFAULT '',
    region          TEXT NOT NULL DEFAULT '',
    resource_type   TEXT NOT NULL,
    resource_id     TEXT NOT NULL,
    name            TEXT NOT NULL DEFAULT '',
    monthly_cost    NUMERIC(12, 2) NOT NULL DEFAULT 0,
    recommendation  TEXT NOT NULL DEFAULT '',
    action          TEXT NOT NULL DEFAULT '',
    risk            TEXT NOT NULL DEFAULT 'low',
    status          TEXT NOT NULL DEFAULT 'open',
    evidence        JSONB NOT NULL DEFAULT '{}',
    iac_repo        TEXT NOT NULL DEFAULT '',
    iac_path        TEXT NOT NULL DEFAULT '',
    terraform_addr  TEXT NOT NULL DEFAULT '',
    first_seen_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, account, region, resource_type, resource_id)
);

CREATE INDEX IF NOT EXISTS cost_opportunities_status
    ON cost_opportunities (status, monthly_cost DESC);

CREATE TABLE IF NOT EXISTS iac_pull_requests (
    id             BIGSERIAL PRIMARY KEY,
    opportunity_id BIGINT NOT NULL REFERENCES cost_opportunities(id) ON DELETE CASCADE,
    actor          TEXT NOT NULL DEFAULT '',
    provider       TEXT NOT NULL DEFAULT 'terraform',
    repo           TEXT NOT NULL DEFAULT '',
    branch         TEXT NOT NULL DEFAULT '',
    title          TEXT NOT NULL DEFAULT '',
    body           TEXT NOT NULL DEFAULT '',
    diff           TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'planned',
    url            TEXT NOT NULL DEFAULT '',
    error          TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS iac_pull_requests_opportunity
    ON iac_pull_requests (opportunity_id, created_at DESC);
