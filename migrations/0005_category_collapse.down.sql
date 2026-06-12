-- 0005_category_collapse.down.sql — rollback v5 collapsible categories.
-- Drop the per-user collapse table. With it gone, every section renders
-- expanded (v4 behavior); nothing else references it, so the rollback is clean.

DROP TABLE IF EXISTS user_collapsed_categories;
