-- 0001_init.up.sql — homepad v1 schema
-- Status: scaffold (RED phase). Tables here will be created during GREEN; the
-- columns may shift slightly as ACs are driven to passing. Keep changes
-- additive once GREEN is reached.

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE IF NOT EXISTS users (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email           CITEXT       NOT NULL UNIQUE,
    password_hash   TEXT         NOT NULL,
    role            TEXT         NOT NULL DEFAULT 'user' CHECK (role IN ('admin', 'user')),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS services (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug            TEXT         NOT NULL UNIQUE,
    name            TEXT         NOT NULL,
    description     TEXT         NOT NULL DEFAULT '',
    url             TEXT         NOT NULL,
    icon            TEXT         NOT NULL DEFAULT '',
    gatus_key       TEXT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS favorites (
    user_id     UUID NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
    service_id  UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, service_id)
);

CREATE TABLE IF NOT EXISTS user_layout (
    user_id     UUID NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
    service_id  UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    sort_index  INTEGER NOT NULL,
    PRIMARY KEY (user_id, service_id)
);

CREATE INDEX IF NOT EXISTS user_layout_by_user_idx
    ON user_layout (user_id, sort_index);
