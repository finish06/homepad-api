-- 0003_theme_pref.up.sql — homepad v3 theme mode (System / Light / Dark)
-- Additive only: one column on users holding the per-user theme preference.
-- NOT NULL DEFAULT 'system' backfills every existing row to the intended
-- default (zero data migration), and the CHECK mirrors the v1/v2 pattern
-- (role, variant) of constraining the enum at the DB layer so a bad value can
-- never persist. The seeded catalog and all other tables are untouched.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS theme_pref TEXT NOT NULL DEFAULT 'system'
        CHECK (theme_pref IN ('system', 'light', 'dark'));
