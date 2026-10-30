-- 0018_kid_accounts.up.sql — kid accounts (SPEC-kid-accounts §6, plan TASK-201).
--
-- A third role, 'kid': created only by an admin, with a display name and an
-- avatar color but no email and no password, who sees only the services on its
-- allow-list (kid_allowed_services).
--
-- users.display_name already exists (0006), so this adds only avatar_color.
--
-- Idempotent: Migrate re-runs every migration on every boot. The constraint
-- swap is guarded on users_kid_credentials_check, so it runs once; every other
-- statement is IF NOT EXISTS.
ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_color TEXT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'users_kid_credentials_check'
          AND conrelid = 'users'::regclass
    ) THEN
        ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
        ALTER TABLE users
            ADD CONSTRAINT users_role_check CHECK (role IN ('admin', 'user', 'kid'));

        -- email and password_hash are NULL exactly when the account is a kid.
        -- Adults keep the NOT NULL guarantee they had (now a CHECK, not a
        -- column property), and a kid can never carry an email — so the two
        -- email-keyed sign-in paths (/api/login, OIDC) can never resolve to a
        -- kid. A kid must have a display name: it is all a kid account has.
        ALTER TABLE users ALTER COLUMN email DROP NOT NULL;
        ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;
        ALTER TABLE users
            ADD CONSTRAINT users_kid_credentials_check CHECK (
                (role = 'kid' AND email IS NULL AND password_hash IS NULL AND display_name IS NOT NULL)
                OR (role <> 'kid' AND email IS NOT NULL AND password_hash IS NOT NULL)
            );

        -- The frontend's accent ids (src/theme/accent.ts). NULL = unset.
        ALTER TABLE users
            ADD CONSTRAINT users_avatar_color_check CHECK (
                avatar_color IS NULL
                OR avatar_color IN ('blue', 'teal', 'green', 'yellow', 'orange', 'red', 'pink', 'purple')
            );
    END IF;
END $$;

-- Kid names are unique among kids, case-insensitively (UX §3). Adults are not
-- constrained: their display names are free-form and may repeat.
CREATE UNIQUE INDEX IF NOT EXISTS users_kid_name_key
    ON users (lower(display_name)) WHERE role = 'kid';

-- The allow-list. Both FKs cascade: deleting a kid drops its list (AC-014);
-- deleting a service drops it from every kid's list (§6).
CREATE TABLE IF NOT EXISTS kid_allowed_services (
    user_id     UUID        NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
    service_id  UUID        NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, service_id)
);

CREATE INDEX IF NOT EXISTS kid_allowed_services_service_idx ON kid_allowed_services (service_id);
