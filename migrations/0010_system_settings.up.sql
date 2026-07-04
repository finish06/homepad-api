-- 0010_system_settings.up.sql — SPEC cap6-uptime-display-toggle §6
--
-- A singleton config table for global, runtime-writable System settings. The
-- first (and today only) setting is show_uptime_display, the admin toggle that
-- gates the per-tile uptime display across the app grid. Runtime-writable is the
-- point (D3): env vars would need cluster access + a redeploy, the wrong UX for a
-- UI toggle.
--
-- The CHECK (id = 1) constraint enforces exactly one row (singleton pattern); the
-- Go layer upserts on id = 1 for every write. A MISSING row (fresh install, no
-- admin action) is treated as all-defaults — show_uptime_display = true — so no
-- seed data is required to preserve today's behavior (D7/AC-008). The DEFAULT TRUE
-- also makes an inserted-but-unspecified row default ON.
--
-- Idempotent (migrations re-run on every boot): CREATE TABLE IF NOT EXISTS.
CREATE TABLE IF NOT EXISTS system_settings (
    id                  INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    show_uptime_display BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
