-- 0013_grid_width_12col.up.sql — SPEC-app-grid §10.4: the 12-column group grid
-- REPLACES the 1–8 tile-count width model (Caleb, 2026-09-12, OQ-3).
--
-- categories.grid_width now holds a 12-column SPAN — one of 3 (quarter),
-- 4 (third), 6 (half) or 12 (full) — instead of a tile count 1–8. Existing
-- values are remapped per the table in SPEC-app-grid §10.4; the width-4 case,
-- which has no exact equivalent, goes to 6 (half — Caleb, 2026-09-13):
--
--   old 1 → 3   old 2 → 4   old 3 → 6   old 4 → 6   old 5 → 12   old 6..8 → 12
--
-- The OLD value is preserved in grid_width_legacy (nullable) so the remap is
-- reversible; nothing reads it. The column default moves 3 → 6 (a half-width
-- box is the new "default 3" — same share of the row).
--
-- Migrations re-run on EVERY boot, so this is guarded: the remap happens only
-- while the span constraint does NOT exist yet. Once it is in place the block
-- is a no-op, so already-migrated spans (3/4/6/12) are never remapped a second
-- time. (0009 is guarded the other way round: it stops re-adding the 1–8 CHECK
-- once this constraint exists.)
ALTER TABLE categories ADD COLUMN IF NOT EXISTS grid_width_legacy INTEGER;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'categories_grid_width_span_check'
          AND conrelid = 'categories'::regclass
    ) THEN
        -- The old 1–8 CHECK must go BEFORE the remap: a span of 12 violates it.
        ALTER TABLE categories DROP CONSTRAINT IF EXISTS categories_grid_width_check;

        UPDATE categories
           SET grid_width_legacy = grid_width,
               grid_width = CASE grid_width
                   WHEN 1 THEN 3
                   WHEN 2 THEN 4
                   WHEN 3 THEN 6
                   WHEN 4 THEN 6
                   WHEN 5 THEN 12
                   ELSE 12
               END
         WHERE grid_width_legacy IS NULL;

        ALTER TABLE categories
            ADD CONSTRAINT categories_grid_width_span_check
            CHECK (grid_width IN (3, 4, 6, 12));
        ALTER TABLE categories ALTER COLUMN grid_width SET DEFAULT 6;
    END IF;
END $$;
