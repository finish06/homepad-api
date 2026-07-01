-- 0008_category_grid_width.down.sql — drop the App Grid box width column.
ALTER TABLE categories DROP COLUMN IF EXISTS grid_width;
