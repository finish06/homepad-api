-- 0008_category_layout.up.sql — SPEC category-pane-width-layout
--
-- Adds the 2D layout model to categories: layout_row groups side-by-side panes,
-- layout_col_order orders within a row, layout_width_pct (10–100) is the pane's
-- share of screen width. Migrations re-run on every boot (Migrate relies on
-- idempotency, not a version table), so the one-time backfill of layout_row from
-- sort_index MUST run ONLY when the column is first added — otherwise a restart
-- would clobber an admin's saved layout (breaking AC7). The guarded DO block
-- makes both the ADD and the backfill idempotent; on a fresh/empty DB it touches
-- 0 rows and the pre-feature stacked full-width layout is preserved (AC9).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'categories' AND column_name = 'layout_row'
    ) THEN
        ALTER TABLE categories ADD COLUMN layout_row INTEGER NOT NULL DEFAULT 0;
        UPDATE categories SET layout_row = sort_index;  -- backfill once (AC9)
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'categories' AND column_name = 'layout_col_order'
    ) THEN
        ALTER TABLE categories ADD COLUMN layout_col_order INTEGER NOT NULL DEFAULT 0;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'categories' AND column_name = 'layout_width_pct'
    ) THEN
        ALTER TABLE categories ADD COLUMN layout_width_pct INTEGER NOT NULL DEFAULT 100;
    END IF;
END $$;
