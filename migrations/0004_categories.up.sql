-- 0004_categories.up.sql — homepad v4 app categories
-- Additive only: a new `categories` table (admin-curated, name-unique,
-- explicitly ordered by sort_index) and a nullable FK on `services`. NULL
-- category_id means Uncategorized — a render-time bucket, never a real row.
-- ON DELETE SET NULL is the heart of delete-behavior: dropping a category
-- un-assigns its apps (they fall to Uncategorized); it never deletes a service.
-- Everything starts fresh per Joe (2026-06-11): no Gatus-group seed here.

CREATE TABLE IF NOT EXISTS categories (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT        NOT NULL UNIQUE,
    sort_index  INTEGER     NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE services
    ADD COLUMN IF NOT EXISTS category_id UUID
        REFERENCES categories(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS services_by_category_idx
    ON services (category_id);
