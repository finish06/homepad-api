-- 0011_click_action.up.sql — SPEC-tile-click-action-20260710 (v23) §3.1
--
-- Per-tile click behavior on the shared catalog. Each service gets a click_action
-- of new_tab (open in a new browser tab — today's hardcoded behavior), same_tab
-- (navigate the current tab), or iframe (open in the in-app IframeOverlay).
--
-- DEFAULT 'new_tab' is the point (AC-001): every pre-existing row — and any row
-- inserted without the field — keeps opening in a new tab exactly as before, with
-- no backfill pass. The inline CHECK enforces the enum at the DB (defence in depth
-- behind the API validator). Idempotent (migrations re-run on every boot):
-- ADD COLUMN IF NOT EXISTS.
--
-- NOTE: the spec named this migration 0008, written before 0008–0010 landed;
-- 0011 is the next free ordinal on main.
ALTER TABLE services
  ADD COLUMN IF NOT EXISTS click_action TEXT NOT NULL DEFAULT 'new_tab'
  CHECK (click_action IN ('new_tab', 'same_tab', 'iframe'));
