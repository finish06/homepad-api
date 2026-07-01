-- 0008_category_grid_width.up.sql — SPEC-app-grid §3B
--
-- Adds the App Grid box width to categories: grid_width (1–6) drives both the
-- box's page-column span and its links-per-row. Migrations re-run on every boot
-- (Migrate relies on idempotency, not a version table), so the ADD is guarded so
-- a restart never clobbers an admin's saved width. New/backfilled rows default to
-- 3 (the spec's default box width). A CHECK keeps the range honest at the DB
-- floor even though the API also validates 1–6.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'categories' AND column_name = 'grid_width'
    ) THEN
        ALTER TABLE categories
            ADD COLUMN grid_width INTEGER NOT NULL DEFAULT 3
            CHECK (grid_width BETWEEN 1 AND 6);
    END IF;
END $$;
