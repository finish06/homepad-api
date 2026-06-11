-- 0003_theme_pref.down.sql — rollback v3 theme mode.
-- Nothing else references theme_pref, so dropping the column reverts the app
-- cleanly to v1/v2 visual behavior (System-only).

ALTER TABLE users DROP COLUMN IF EXISTS theme_pref;
