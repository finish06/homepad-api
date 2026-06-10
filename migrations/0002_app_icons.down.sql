-- 0002_app_icons.down.sql — rollback v2 custom app icons.
-- Nothing else references service_icons and services.icon was never removed,
-- so dropping this table reverts the app cleanly to v1 icon behavior.

DROP TABLE IF EXISTS service_icons;
