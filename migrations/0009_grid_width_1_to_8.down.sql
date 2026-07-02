-- 0009_grid_width_1_to_8.down.sql — revert the A1 range widening back to 1–6.
-- (Fails if any row already stores a width of 7 or 8 — narrow the data first.)
ALTER TABLE categories DROP CONSTRAINT IF EXISTS categories_grid_width_check;
ALTER TABLE categories ADD CONSTRAINT categories_grid_width_check CHECK (grid_width BETWEEN 1 AND 6);
