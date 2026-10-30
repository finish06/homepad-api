-- Back to two roles: every kid account (and, by cascade, its sessions,
-- favorites, preferences and allow-list) is deleted.
DROP TABLE IF EXISTS kid_allowed_services;
DROP INDEX IF EXISTS users_kid_name_key;
DELETE FROM users WHERE role = 'kid';
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_avatar_color_check;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_kid_credentials_check;
ALTER TABLE users ALTER COLUMN email SET NOT NULL;
ALTER TABLE users ALTER COLUMN password_hash SET NOT NULL;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check CHECK (role IN ('admin', 'user'));
ALTER TABLE users DROP COLUMN IF EXISTS avatar_color;
