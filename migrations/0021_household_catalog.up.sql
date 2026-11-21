-- 0021_household_catalog.up.sql — the catalog belongs to the household
-- (SPEC-household-catalog-owner §6.2, homepad 18.0.0).
--
-- Sections and apps stop belonging to the first admin. Rows owned by anyone
-- else were never shown on any dashboard (v9-era per-user copies, and what a
-- second admin "created" before #87); they are copied to catalog_archive and
-- deleted. Then services.user_id and categories.user_id are dropped, and slugs
-- and section names become unique across the table.
--
-- ONE WAY. There is no working down migration: to go back, restore the
-- database dump taken before the upgrade (see the CHANGELOG).
--
-- It runs once: the runner records it in schema_migrations (§6.1). It is also
-- guarded on services.user_id, so running the file again is a no-op.

DO $$
DECLARE
    owner_id      UUID;
    archived_svcs INTEGER;
    archived_cats INTEGER;
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
         WHERE table_schema = current_schema() AND table_name = 'services' AND column_name = 'user_id'
    ) THEN
        RETURN;
    END IF;

    -- 1) The owner: the same rule 17.x used to read the catalog.
    SELECT id INTO owner_id FROM users WHERE role = 'admin' ORDER BY created_at, id LIMIT 1;

    -- 2) Catalog rows and no admin: nobody can say whose rows are the catalog.
    --    Stop, and change nothing (Q4).
    IF owner_id IS NULL AND (EXISTS (SELECT 1 FROM services) OR EXISTS (SELECT 1 FROM categories)) THEN
        RAISE EXCEPTION 'migration 0021: the catalog has apps but no admin account exists. Make one account an admin, then restart.';
    END IF;

    -- 3) The archive. Nothing reads it; a later major release drops it. The
    --    former owner has no foreign key: the account may be deleted later.
    CREATE TABLE IF NOT EXISTS catalog_archive (
        id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
        kind                TEXT        NOT NULL CHECK (kind IN ('service', 'category')),
        former_owner_id     UUID,
        former_owner_email  TEXT,
        row                 JSONB       NOT NULL,
        archived_at         TIMESTAMPTZ NOT NULL DEFAULT now()
    );

    -- 4) Copy every row that isn't the owner's, then delete it. The foreign
    --    keys cascade to its icons, widget, favorites, order, folded sections
    --    and allow-list rows; none of them were visible. Widget rows are not
    --    copied: their keys are sealed with HOMEPAD_SECRET_KEY.
    INSERT INTO catalog_archive (kind, former_owner_id, former_owner_email, row)
    SELECT 'service', s.user_id, u.email::text, to_jsonb(s)
      FROM services s LEFT JOIN users u ON u.id = s.user_id
     WHERE s.user_id IS DISTINCT FROM owner_id;
    GET DIAGNOSTICS archived_svcs = ROW_COUNT;

    INSERT INTO catalog_archive (kind, former_owner_id, former_owner_email, row)
    SELECT 'category', c.user_id, u.email::text, to_jsonb(c)
      FROM categories c LEFT JOIN users u ON u.id = c.user_id
     WHERE c.user_id IS DISTINCT FROM owner_id;
    GET DIAGNOSTICS archived_cats = ROW_COUNT;

    DELETE FROM services   WHERE user_id IS DISTINCT FROM owner_id;
    DELETE FROM categories WHERE user_id IS DISTINCT FROM owner_id;

    RAISE NOTICE 'migration 0021: archived % services and % sections (catalog_archive)', archived_svcs, archived_cats;

    -- 5) The per-owner indexes.
    DROP INDEX IF EXISTS services_user_slug_key;
    DROP INDEX IF EXISTS services_by_user_idx;
    DROP INDEX IF EXISTS categories_user_name_key;
    DROP INDEX IF EXISTS categories_by_user_idx;

    -- 6) The owner columns, and with them the foreign keys to users. Deleting
    --    an account can no longer delete the catalog.
    ALTER TABLE services   DROP COLUMN user_id;
    ALTER TABLE categories DROP COLUMN user_id;

    -- 7) Unique across the table. This can't fail on duplicates: only the
    --    owner's rows are left, and they were unique per owner. The names
    --    don't collide with services_slug_key / categories_name_key, which
    --    0001 and 0004 create and 0007 drops.
    CREATE UNIQUE INDEX IF NOT EXISTS services_slug_unique   ON services (slug);
    CREATE UNIQUE INDEX IF NOT EXISTS categories_name_unique ON categories (name);
    CREATE INDEX        IF NOT EXISTS categories_sort_idx    ON categories (sort_index);
END $$;
