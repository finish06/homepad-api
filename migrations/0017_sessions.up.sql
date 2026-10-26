-- 0017_sessions.up.sql — persistent, hashed sessions (SPEC-kid-accounts §6,
-- AC-010, AC-011; plan TASK-102). Replaces the in-memory session map, so a
-- restart or deploy no longer signs everyone out.
--
-- Idempotent: Migrate re-runs every migration on every boot, so this must never
-- drop or empty the table — doing so would sign everyone out on every deploy,
-- which is the exact bug this migration fixes.
--
-- token_hash is sha256(token), never the token itself (AC-011). The CHECK pins
-- it to a 32-byte digest, so a code path that tried to store a raw token would
-- fail loudly instead of quietly persisting a credential.
--
-- handle is an opaque random id for revoke-by-handle (the Phase 3 device list).
-- It is generated independently of the token and is never derived from it, so
-- showing a handle reveals nothing that authenticates.
CREATE TABLE IF NOT EXISTS sessions (
    token_hash    BYTEA        PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    handle        TEXT         NOT NULL UNIQUE,
    user_id       UUID         NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ  NOT NULL,
    user_agent    TEXT         NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS sessions_user_id_idx    ON sessions (user_id);
CREATE INDEX IF NOT EXISTS sessions_expires_at_idx ON sessions (expires_at);
