-- 0003_db_surface: M3 databases unify into the existing model (ADR-030).
-- DB instances are workloads with source='db'; class recommendations ride
-- recommendations.resource='class' with class_current/class_proposed; class
-- diffs ride apply_events.diff (JSONB — no schema change there).

ALTER TABLE workloads
    ADD COLUMN IF NOT EXISTS db_class              TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS db_replicas           INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS db_maintenance_window TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS db_provider           TEXT NOT NULL DEFAULT '';

ALTER TABLE recommendations
    ADD COLUMN IF NOT EXISTS class_current  TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS class_proposed TEXT NOT NULL DEFAULT '';
