-- 0008_category_layout.down.sql — drop the category layout columns.
ALTER TABLE categories
    DROP COLUMN IF EXISTS layout_row,
    DROP COLUMN IF EXISTS layout_col_order,
    DROP COLUMN IF EXISTS layout_width_pct;
