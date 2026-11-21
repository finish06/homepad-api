-- 0022_password_resets.up.sql — one-time password reset links
-- (SPEC-password-reset-link §6).
--
-- An admin (or the reset-link CLI, created_by NULL) makes a link for an
-- adult with a local password. The adult opens it, picks a new password,
-- and every session of theirs ends. Making a new link revokes the earlier
-- live one.
--
-- token_hash is sha256(token), never the token. The CHECK pins it to a
-- 32-byte digest, as on invites and sessions.
--
-- "Live" is used_at IS NULL AND revoked_at IS NULL AND expires_at > now().
-- Used, revoked and expired rows stay 30 days for the audit trail, then the
-- hourly sweep deletes them. Removing the account removes its links.
--
-- The runner applies this once (schema_migrations); it is still written to
-- be re-run.
CREATE TABLE IF NOT EXISTS password_resets (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash  BYTEA       NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_by  UUID        REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS password_resets_live_idx ON password_resets (user_id)
    WHERE used_at IS NULL AND revoked_at IS NULL;
