-- 0023_iframe_same_origin.up.sql — the overlay's per-app sign-in opt-in
-- (SPEC-iframe-sandbox-optin §6).
--
-- The overlay drops allow-same-origin from its sandbox unless an admin turns
-- on "Let this app keep its sign-in" for that app. This column is that
-- switch. New apps start with it off.
--
-- Q1 (decided 2026-11-19, "keep them opt in"): apps already set to Inline
-- overlay keep today's behavior, so the upgrade turns it on for them.
--
-- Numbering: 0021 is household-catalog-owner and 0022 is the reset link.
-- The runner (storage.Migrate) records each file in schema_migrations and
-- runs it once. The column add still checks first, and the backfill runs
-- only in the batch that adds the column, so a re-run (a lost record) never
-- turns an admin's "off" back on.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema() AND table_name = 'services'
          AND column_name = 'iframe_same_origin'
    ) THEN
        ALTER TABLE services ADD COLUMN iframe_same_origin BOOLEAN NOT NULL DEFAULT FALSE;
        -- Q1 (decided 2026-11-19): apps already on Inline overlay keep today's behavior.
        UPDATE services SET iframe_same_origin = TRUE WHERE click_action = 'iframe';
    END IF;
END $$;
