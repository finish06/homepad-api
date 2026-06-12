-- 0006_display_name.down.sql — drop the v7 display name column.
ALTER TABLE users
    DROP COLUMN IF EXISTS display_name;
