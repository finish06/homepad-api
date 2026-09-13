-- 0012_density_pref.down.sql — rollback v16 per-user tile density.
-- Nothing else references density_pref; the frontend falls back to its
-- per-device localStorage value when the field is absent.

ALTER TABLE users DROP COLUMN IF EXISTS density_pref;
