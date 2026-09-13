-- 0014_status_degraded_ms.up.sql — the "Slow" threshold becomes a runtime
-- System setting (Caleb, 2026-09-13: v16 UI settings must be manageable in the
-- UI, not only via env). NULL = "not set in the UI": the server falls back to
-- GATUS_DEGRADED_MS / its 1000 ms default, so existing installs change nothing.
-- Idempotent (migrations re-run on every boot).
ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS status_degraded_ms INTEGER
    CHECK (status_degraded_ms IS NULL OR (status_degraded_ms >= 0 AND status_degraded_ms <= 600000));
