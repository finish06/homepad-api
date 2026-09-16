-- 0015_show_health_bar.up.sql — SPEC-health-bar-visibility-toggle AC-001:
-- per-user visibility of the health panel's distribution bar.
--
-- NOT NULL DEFAULT TRUE makes "default visible" a property of the schema rather
-- than of client code: every existing row is visible-by-default at migration
-- time, so no user's dashboard changes on deploy.
--
-- Idempotent (migrations re-run on every boot).
ALTER TABLE users ADD COLUMN IF NOT EXISTS show_health_bar BOOLEAN NOT NULL DEFAULT TRUE;
