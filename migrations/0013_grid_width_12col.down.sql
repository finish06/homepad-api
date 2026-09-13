-- 0013_grid_width_12col.down.sql — revert to the 1–8 tile-count model using the
-- preserved legacy values (rows created after the migration have no legacy
-- value; they get the old default 3).
ALTER TABLE categories DROP CONSTRAINT IF EXISTS categories_grid_width_span_check;
UPDATE categories SET grid_width = COALESCE(grid_width_legacy, 3);
ALTER TABLE categories ALTER COLUMN grid_width SET DEFAULT 3;
ALTER TABLE categories ADD CONSTRAINT categories_grid_width_check CHECK (grid_width BETWEEN 1 AND 8);
ALTER TABLE categories DROP COLUMN IF EXISTS grid_width_legacy;
