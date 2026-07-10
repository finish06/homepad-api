-- 0011_click_action.down.sql — revert v23 per-tile click action.
-- Drops the column (and its CHECK constraint with it). Tiles revert to the
-- hardcoded new-tab behavior.
ALTER TABLE services DROP COLUMN IF EXISTS click_action;
