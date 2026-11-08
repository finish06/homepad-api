-- 0020_widgets.up.sql — status widgets (SPEC-status-widgets §9).
--
-- One widget per catalog service (service_widgets), the clock, weather and
-- web search settings on the system_settings singleton, and the per-user
-- "Show widgets" switch.
--
-- Numbering: 0019 is reserved for household-basics. The migration runner
-- (storage.Migrate) keeps no version table: it runs every *.up.sql in lexical
-- order on every boot, so a gap in the numbers is harmless, and this file
-- depends on nothing 0019 creates.
--
-- Idempotent: every statement is IF NOT EXISTS.

-- The kind is validated against the Go provider registry, not a CHECK, so a
-- new provider needs no migration. The key is AES-256-GCM ciphertext (never
-- plaintext); its nonce lives beside it, and the two are NULL together.
CREATE TABLE IF NOT EXISTS service_widgets (
    service_id        UUID        PRIMARY KEY REFERENCES services(id) ON DELETE CASCADE,
    kind              TEXT        NOT NULL,
    address           TEXT        NOT NULL DEFAULT '',  -- '' = use services.url
    username          TEXT        NOT NULL DEFAULT '',
    secret_ciphertext BYTEA,                            -- NULL = no key
    secret_nonce      BYTEA,
    fields            TEXT[]      NOT NULL DEFAULT '{}',
    interval_s        INTEGER     NOT NULL DEFAULT 60 CHECK (interval_s IN (60, 300, 900)),
    insecure_tls      BOOLEAN     NOT NULL DEFAULT FALSE,
    enabled           BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((secret_ciphertext IS NULL) = (secret_nonce IS NULL)),
    CHECK (cardinality(fields) <= 3)
);

-- Clock and weather start off (AC-035), so an install with no widgets looks
-- exactly as before. Coordinates are stored rounded to 2 decimals (AC-028).
ALTER TABLE system_settings
    ADD COLUMN IF NOT EXISTS clock_enabled      BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS clock_hour12       BOOLEAN,              -- NULL = the device's choice
    ADD COLUMN IF NOT EXISTS weather_enabled    BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS weather_place      TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS weather_lat        NUMERIC(5,2),
    ADD COLUMN IF NOT EXISTS weather_lon        NUMERIC(5,2),
    ADD COLUMN IF NOT EXISTS weather_units      TEXT    NOT NULL DEFAULT 'c' CHECK (weather_units IN ('c', 'f')),
    ADD COLUMN IF NOT EXISTS web_search_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS web_search_url     TEXT    NOT NULL DEFAULT 'https://duckduckgo.com/?q={query}',
    ADD COLUMN IF NOT EXISTS kid_web_search     BOOLEAN NOT NULL DEFAULT FALSE;

-- On by default, as a property of the schema: no user's dashboard changes on
-- deploy (AC-031).
ALTER TABLE users ADD COLUMN IF NOT EXISTS show_widgets BOOLEAN NOT NULL DEFAULT TRUE;
