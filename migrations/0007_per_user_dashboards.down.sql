-- 0007_per_user_dashboards.down.sql — best-effort rollback to the v5 schema.
--
-- LOSSY BY NATURE (D9): per-user apps/categories created AFTER the cutover
-- collapse into one global namespace and may COLLIDE on the re-globalized
-- UNIQUE constraints — restoring services_slug_key / categories_name_key below
-- will then FAIL. This down is provided for DEV RESETS of a scratch DB, not for
-- production reversal; v9 is forward-only in practice.

DROP INDEX IF EXISTS services_user_slug_key;
DROP INDEX IF EXISTS services_by_user_idx;
DROP INDEX IF EXISTS categories_user_name_key;
DROP INDEX IF EXISTS categories_by_user_idx;

-- Restore the global UNIQUE constraints (only succeeds with no cross-user dups).
ALTER TABLE services   ADD CONSTRAINT services_slug_key   UNIQUE (slug);
ALTER TABLE categories ADD CONSTRAINT categories_name_key UNIQUE (name);

ALTER TABLE services   DROP COLUMN IF EXISTS source_library_id;
ALTER TABLE services   DROP COLUMN IF EXISTS user_id;
ALTER TABLE categories DROP COLUMN IF EXISTS user_id;

DROP TABLE IF EXISTS library_apps;
