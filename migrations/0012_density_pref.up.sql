-- 0012_density_pref.up.sql — v16 tile density, per user (homepad decision record
-- 2026-09-12, OQ-9: "per user, server-side"). Additive only: one column on users
-- holding the dashboard density. NOT NULL DEFAULT 'compact' backfills every
-- existing row to the v16 default (zero data migration); the CHECK mirrors the
-- theme_pref pattern of constraining the enum at the DB layer.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS density_pref TEXT NOT NULL DEFAULT 'compact'
        CHECK (density_pref IN ('large', 'compact', 'list'));
