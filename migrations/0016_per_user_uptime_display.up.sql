-- 0016_per_user_uptime_display.up.sql — cap6 v2: "Show uptime display" becomes a
-- per-user preference (specs/cap6-uptime-display-toggle.md §6).
--
-- Idempotent AND once-only. Migrate re-runs EVERY migration on EVERY boot, so the
-- seeding UPDATE cannot sit at the top level: unguarded, it would reset every
-- user's choice back to the global value on every API restart (AC-021).
--
-- The guard predicate is the COLUMN — the vehicle this migration adds. NOTE this
-- is NOT the same predicate as 0013, which guards on pg_constraint, i.e. on its
-- own EFFECT, and adds grid_width_legacy unguarded at top level. Both are correct
-- for what they do; do not copy one to the other without re-deciding which you
-- need.
--
-- table_schema is pinned to current_schema(): an unqualified information_schema
-- lookup would match a `users` in ANY schema, flip NOT EXISTS false, skip the
-- whole block, never create the column, and fail hard on the first query. 0013
-- sidesteps this with ::regclass.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'users'
          AND column_name = 'show_uptime_display'
    ) THEN
        -- DEFAULT TRUE is needed here so the column can be NOT NULL over existing
        -- rows. It is dropped again below.
        ALTER TABLE users
            ADD COLUMN show_uptime_display BOOLEAN NOT NULL DEFAULT TRUE;

        -- AC-020 — inherit whatever the admin had set globally, so nobody's view
        -- changes. COALESCE covers the fresh-install case with no settings row.
        UPDATE users
           SET show_uptime_display = COALESCE(
                 (SELECT show_uptime_display FROM system_settings WHERE id = 1), TRUE);

        -- TWO SOURCES OF DEFAULT TRUTH — the AC-027 failure mode. Leaving DEFAULT
        -- TRUE in place means the column default and system_settings both claim to
        -- decide a new account's value; an insert path that omits the field would
        -- silently get TRUE and still look right. Invisible while the admin default
        -- is ON; surfaces only when it is OFF and a new account comes up ON.
        -- Dropping it makes that a loud NOT NULL violation at insert time and
        -- leaves system_settings as the single source of truth (AC-026).
        ALTER TABLE users ALTER COLUMN show_uptime_display DROP DEFAULT;
    END IF;
END $$;
