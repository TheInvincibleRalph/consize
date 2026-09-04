-- 0010_recommendation_step_progress: preserve multi-step chain progress.
--
-- A stepped apply queues a follow-up recommendation for the remainder.
-- Without step metadata on that follow-up, applying it records a fresh
-- "step 1/N" instead of "step 2/original-total". These columns let the
-- apply engines keep the audit timeline coherent across continuation
-- recommendations while leaving fresh analysis rows as new plans.

ALTER TABLE recommendations
    ADD COLUMN IF NOT EXISTS step_number INT NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS total_steps INT NOT NULL DEFAULT 0;

WITH candidates AS (
    SELECT DISTINCT ON (r.id)
        r.id,
        ae.step_number + 1 AS next_step_number,
        ae.total_steps
    FROM recommendations r
    JOIN apply_events ae
        ON ae.workload_id = r.workload_id
       AND ae.result = 'applied'
       AND ae.step_number < ae.total_steps
       AND r.resource = ae.diff->>'resource'
    WHERE r.status = 'pending'
      AND r.step_number = 1
      AND r.total_steps = 0
      AND r.created_at >= ae.created_at
      AND (
          (r.resource = 'class' AND r.class_current = ae.diff->>'proposed_class')
          OR
          (r.resource <> 'class' AND r.current_value = (ae.diff->>'proposed_request')::BIGINT)
      )
    ORDER BY r.id, ae.created_at DESC
)
UPDATE recommendations r
SET step_number = candidates.next_step_number,
    total_steps = candidates.total_steps
FROM candidates
WHERE r.id = candidates.id;
