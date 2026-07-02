-- 0009_grid_width_1_to_8.up.sql — SPEC-app-grid Amendment A1
--
-- Widens the categories.grid_width range from 1–6 to 1–8. Migration 0008 added
-- the column with an inline CHECK (grid_width BETWEEN 1 AND 6), which Postgres
-- auto-named `categories_grid_width_check`; 0008's guard is `IF NOT EXISTS column`,
-- so editing 0008 in place would NOT re-apply once the column exists on a running
-- DB. This separate migration drops the old constraint and re-adds it at 1–8.
-- Idempotent (migrations re-run on every boot): the DROP IF EXISTS + ADD pair
-- lands the same 1–8 constraint no matter how many times it runs, and it is safe
-- to widen a range with existing rows in it. Matches the API validator (1–8).
ALTER TABLE categories DROP CONSTRAINT IF EXISTS categories_grid_width_check;
ALTER TABLE categories ADD CONSTRAINT categories_grid_width_check CHECK (grid_width BETWEEN 1 AND 8);
