-- 0019_invites.up.sql — invite links for adding an adult
-- (SPEC-household-basics §6).
--
-- An admin makes an invite with a name and a role; the adult opens the link,
-- picks an email and a password, and the account is created then. No users
-- row exists until the invite is accepted, so users_kid_credentials_check
-- (adults have an email and a password hash) holds without a placeholder.
--
-- token_hash is sha256(token), never the token (§3.2). The CHECK pins it to a
-- 32-byte digest, as on sessions, so a code path that tried to store a raw
-- token fails loudly.
--
-- "Pending" is used_at IS NULL AND revoked_at IS NULL AND expires_at > now().
-- Used, revoked and expired rows stay 30 days for the audit trail, then the
-- hourly sweep deletes them.
--
-- Idempotent: Migrate re-runs every migration on every boot.
CREATE TABLE IF NOT EXISTS invites (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash  BYTEA       NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    name        TEXT        NOT NULL,
    role        TEXT        NOT NULL CHECK (role IN ('admin', 'user')),
    created_by  UUID        REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    used_by     UUID        REFERENCES users(id) ON DELETE SET NULL,
    revoked_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS invites_pending_idx ON invites (expires_at)
    WHERE used_at IS NULL AND revoked_at IS NULL;
