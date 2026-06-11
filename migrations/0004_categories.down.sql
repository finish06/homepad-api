-- 0004_categories.down.sql — rollback v4 app categories.
-- Drop the FK column (and its index) first, then the table. With category_id
-- gone the catalog reverts cleanly to v1's flat render; no service rows are
-- modified destructively, so nothing is lost beyond the grouping.

DROP INDEX IF EXISTS services_by_category_idx;
ALTER TABLE services DROP COLUMN IF EXISTS category_id;
DROP TABLE IF EXISTS categories;
