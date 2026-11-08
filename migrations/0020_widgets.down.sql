-- Removes status widgets: every widget (and its sealed key), the widget
-- settings, and the per-user switch.
DROP TABLE IF EXISTS service_widgets;
ALTER TABLE system_settings
    DROP COLUMN IF EXISTS clock_enabled,
    DROP COLUMN IF EXISTS clock_hour12,
    DROP COLUMN IF EXISTS weather_enabled,
    DROP COLUMN IF EXISTS weather_place,
    DROP COLUMN IF EXISTS weather_lat,
    DROP COLUMN IF EXISTS weather_lon,
    DROP COLUMN IF EXISTS weather_units,
    DROP COLUMN IF EXISTS web_search_enabled,
    DROP COLUMN IF EXISTS web_search_url,
    DROP COLUMN IF EXISTS kid_web_search;
ALTER TABLE users DROP COLUMN IF EXISTS show_widgets;
