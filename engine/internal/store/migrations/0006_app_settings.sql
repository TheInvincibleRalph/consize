-- 0006_app_settings: durable installation/product configuration.
--
-- Used first by E1.9 alert routing. Values are plain configuration JSON, not
-- secrets: webhook tokens and provider credentials are referenced through
-- Kubernetes Secret-backed environment variables.

CREATE TABLE IF NOT EXISTS app_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
