-- 0007_per_user_dashboards.up.sql — homepad v9.1 per-user cutover
--
-- The biggest migration in homepad's history: the single shared global catalog
-- becomes per-user dashboards + an admin-curated App Library. This is a one-way
-- data transform (see specs/v9-per-user-dashboards.md §5.5). It is written to be
-- safe + idempotent whether the catalog is EMPTY (fresh/test DB) or holds the
-- production services. Steps 3/5/5b touch 0 rows on an empty DB; step 6's
-- SET NOT NULL then succeeds vacuously.
--
-- NOTE on numbering: the spec drafted this as "0006", but 0006_display_name
-- (v7) landed first, so the real file is 0007.

-- 1) The shared, admin-curated App Library (offers, not assignments).
CREATE TABLE IF NOT EXISTS library_apps (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name               TEXT        NOT NULL,
    url                TEXT        NOT NULL,
    icon               TEXT        NOT NULL DEFAULT '',
    description        TEXT        NOT NULL DEFAULT '',
    suggested_category TEXT        NOT NULL DEFAULT '',   -- free-text hint, NOT a FK (D5)
    sort_index         INTEGER     NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 2) Ownership + provenance columns, NULLABLE for now (backfilled in step 5,
--    made NOT NULL in step 6).
ALTER TABLE services
    ADD COLUMN IF NOT EXISTS user_id UUID REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE services
    ADD COLUMN IF NOT EXISTS source_library_id UUID
        REFERENCES library_apps(id) ON DELETE SET NULL;   -- provenance ONLY (C1)
ALTER TABLE categories
    ADD COLUMN IF NOT EXISTS user_id UUID REFERENCES users(id) ON DELETE CASCADE;

-- 3) Seed the Library from the existing shared services (C3). One offer per
--    service; suggested_category = its v4 category name (or '' if uncategorized).
--    Guarded so re-running the migration does not double-seed.
INSERT INTO library_apps (name, url, icon, description, suggested_category, sort_index)
SELECT s.name, s.url, s.icon, s.description, COALESCE(c.name, ''),
       (ROW_NUMBER() OVER (ORDER BY s.name)) - 1
FROM services s
LEFT JOIN categories c ON c.id = s.category_id
WHERE NOT EXISTS (SELECT 1 FROM library_apps)          -- idempotent: only seed once
  AND s.user_id IS NULL;                               -- only pre-cutover (unowned) rows

-- 5) Reassign the existing shared services + categories to the FIRST admin
--    (role='admin' ORDER BY created_at) so that dashboard survives the cutover
--    as that admin's personal one. On an empty DB or one with no admin yet,
--    these UPDATEs touch 0 rows.
UPDATE services
   SET user_id = (SELECT id FROM users WHERE role='admin' ORDER BY created_at, id LIMIT 1)
 WHERE user_id IS NULL;
UPDATE categories
   SET user_id = (SELECT id FROM users WHERE role='admin' ORDER BY created_at, id LIMIT 1)
 WHERE user_id IS NULL;

-- 5b) Wire provenance (best-effort, non-critical — C1): link each reassigned
--     copy back to the offer minted from it. Match on (name,url), 1:1 by
--     construction in step 3.
UPDATE services sv
   SET source_library_id = la.id
  FROM library_apps la
 WHERE sv.source_library_id IS NULL AND sv.name = la.name AND sv.url = la.url;

-- 6) Enforce: NOT NULL on user_id, swap global unique → per-user unique, index.
--    If services/categories rows exist but NO admin user does, SET NOT NULL
--    fails LOUDLY — a catalog with no possible owner is a misconfiguration.
ALTER TABLE services   ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE categories ALTER COLUMN user_id SET NOT NULL;

ALTER TABLE services   DROP CONSTRAINT IF EXISTS services_slug_key;
ALTER TABLE categories DROP CONSTRAINT IF EXISTS categories_name_key;

CREATE UNIQUE INDEX IF NOT EXISTS services_user_slug_key   ON services   (user_id, slug);
CREATE INDEX        IF NOT EXISTS services_by_user_idx     ON services   (user_id);
CREATE UNIQUE INDEX IF NOT EXISTS categories_user_name_key ON categories (user_id, name);
CREATE INDEX        IF NOT EXISTS categories_by_user_idx   ON categories (user_id, sort_index);
