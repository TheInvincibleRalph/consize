-- 0011_repair_step_chain_metadata: repair step metadata after analyzer resets.
--
-- Before 0010/this release, analyze could supersede a pending continuation
-- after verification passed. The next apply still patched the right live
-- value, but it recorded a fresh "1/N" step because the continuation metadata
-- had been erased. Recover the metadata by matching the applied event's
-- current value to the most recent superseded continuation for the same
-- workload/resource.
--
-- This only corrects derived step fields. It does not change the resource
-- diff, actor, timestamp, result, or verification verdict.

WITH repaired_events AS (
    SELECT DISTINCT ON (ae.id)
        ae.id,
        r.step_number,
        r.total_steps
    FROM apply_events ae
    JOIN recommendations r
      ON r.workload_id = ae.workload_id
     AND r.resource = ae.diff->>'resource'
     AND r.status = 'superseded'
     AND r.step_number > 1
     AND r.total_steps >= r.step_number
     AND r.created_at <= ae.created_at
    WHERE ae.result = 'applied'
      AND ae.step_number = 1
      AND ae.total_steps < r.total_steps
      AND (
          (r.resource = 'class' AND r.class_current = ae.diff->>'current_class')
          OR
          (r.resource <> 'class' AND r.current_value = (ae.diff->>'current_request')::BIGINT)
      )
    ORDER BY ae.id, r.created_at DESC
)
UPDATE apply_events ae
SET step_number = repaired_events.step_number,
    total_steps = repaired_events.total_steps
FROM repaired_events
WHERE ae.id = repaired_events.id;

WITH repaired_pending AS (
    SELECT DISTINCT ON (r.id)
        r.id,
        ae.step_number + 1 AS step_number,
        ae.total_steps
    FROM recommendations r
    JOIN apply_events ae
      ON ae.workload_id = r.workload_id
     AND ae.result = 'applied'
     AND ae.step_number < ae.total_steps
     AND r.resource = ae.diff->>'resource'
     AND r.created_at >= ae.created_at
    WHERE r.status = 'pending'
      AND r.total_steps <= ae.total_steps
      AND (
          (r.resource = 'class' AND r.class_current = ae.diff->>'proposed_class')
          OR
          (r.resource <> 'class' AND r.current_value = (ae.diff->>'proposed_request')::BIGINT)
      )
    ORDER BY r.id, ae.created_at DESC
)
UPDATE recommendations r
SET step_number = repaired_pending.step_number,
    total_steps = repaired_pending.total_steps
FROM repaired_pending
WHERE r.id = repaired_pending.id;
