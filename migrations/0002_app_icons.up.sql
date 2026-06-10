-- 0002_app_icons.up.sql — homepad v2 custom app icons
-- Additive only: introduces per-service uploaded PNG icons (light/dark).
-- The legacy services.icon text column is untouched and remains the
-- lower-precedence fallback (see specs/v2-app-icons.md). A separate table keeps
-- the hot GET /api/services list query from ever pulling blob bytes and models
-- "0, 1, or 2 icons" naturally. ON DELETE CASCADE drops icons with the service.

CREATE TABLE IF NOT EXISTS service_icons (
    service_id  UUID    NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    variant     TEXT    NOT NULL CHECK (variant IN ('light', 'dark')),
    bytes       BYTEA   NOT NULL,
    byte_size   INTEGER NOT NULL,
    width       INTEGER NOT NULL,
    height      INTEGER NOT NULL,
    etag        TEXT    NOT NULL,            -- hex SHA-256 of bytes
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (service_id, variant)
);
