-- 0006_display_name.up.sql — homepad v7 ux-redesign (§6.2/§11)
-- The redesigned avatar renders real user initials, which need a human name.
-- Additive only: one nullable column on users. Nullable (no DEFAULT) means
-- every existing row stays NULL — no backfill, no behavior change — and the
-- frontend falls back to the email's first letter until a name is set.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS display_name TEXT;
