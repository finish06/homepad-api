-- 0005_category_collapse.up.sql — homepad v5 collapsible categories
-- Additive only: one small per-user table holding the set of categories a user
-- has *collapsed*. A row means "this user has folded this category"; absence is
-- the default (expanded), so a brand-new user and a never-touched category both
-- have no rows — zero data migration, and a newly-created category is expanded
-- automatically (it's simply in no one's collapsed set).
-- Both FKs cascade on delete: dropping a user clears their collapse prefs, and
-- dropping a category (v4) clears everyone's collapse row for it — so there is
-- never an orphan pointing at a category that no longer exists, and no cleanup
-- code. Keying on category_id (not name) makes rename/reorder invisible to it.
-- Depends on 0004: this FKs `categories`, so it must run after that migration.

CREATE TABLE IF NOT EXISTS user_collapsed_categories (
    user_id     UUID NOT NULL REFERENCES users(id)      ON DELETE CASCADE,
    category_id UUID NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, category_id)
);
