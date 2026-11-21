-- Removes the overlay's per-app sign-in opt-in (SPEC-iframe-sandbox-optin).
ALTER TABLE services DROP COLUMN IF EXISTS iframe_same_origin;
