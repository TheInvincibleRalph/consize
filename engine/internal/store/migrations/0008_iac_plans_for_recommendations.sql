-- 0008_iac_plans_for_recommendations: make IaC PR plans generic.
--
-- The initial IaC table was introduced for cloud-waste opportunities. The same
-- workflow also applies to normal rightsizing recommendations: teams using
-- infrastructure-as-code should be able to create a PR plan instead of directly
-- patching the cluster/cloud and creating drift.

ALTER TABLE iac_pull_requests
    ALTER COLUMN opportunity_id DROP NOT NULL;

ALTER TABLE iac_pull_requests
    ADD COLUMN IF NOT EXISTS recommendation_id BIGINT REFERENCES recommendations(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS change_kind TEXT NOT NULL DEFAULT 'cost_opportunity';

CREATE INDEX IF NOT EXISTS iac_pull_requests_recommendation
    ON iac_pull_requests (recommendation_id, created_at DESC);

ALTER TABLE iac_pull_requests
    DROP CONSTRAINT IF EXISTS iac_pull_requests_one_source;

ALTER TABLE iac_pull_requests
    ADD CONSTRAINT iac_pull_requests_one_source
    CHECK (
        (opportunity_id IS NOT NULL AND recommendation_id IS NULL AND change_kind = 'cost_opportunity')
        OR
        (opportunity_id IS NULL AND recommendation_id IS NOT NULL AND change_kind = 'recommendation')
    );
