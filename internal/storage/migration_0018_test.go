package storage_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// Migration 0018 — kid accounts. Migrations re-run on EVERY boot, so 0018 must
// upgrade a pre-0018 database without touching existing adults, and then be a
// no-op on every later boot. Same exercise shape as migration_0016_test: put
// the schema back into its pre-0018 form, migrate, assert, migrate again.
func TestMigration0018_UpgradesInPlaceAndIsIdempotent(t *testing.T) {
	dsn := requireDatabaseURL(t)
	ctx := context.Background()
	store, err := storage.Open(ctx, dsn)
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.Migrate(ctx))

	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx, `TRUNCATE categories, users, system_settings CASCADE`)
	require.NoError(t, err)

	// PRE-0018 shape.
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS kid_allowed_services`,
		`DROP INDEX IF EXISTS users_kid_name_key`,
		`ALTER TABLE users DROP CONSTRAINT IF EXISTS users_kid_credentials_check`,
		`ALTER TABLE users DROP CONSTRAINT IF EXISTS users_avatar_color_check`,
		`ALTER TABLE users DROP COLUMN IF EXISTS avatar_color`,
		`ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check`,
		`ALTER TABLE users ADD CONSTRAINT users_role_check CHECK (role IN ('admin', 'user'))`,
		`ALTER TABLE users ALTER COLUMN email SET NOT NULL`,
		`ALTER TABLE users ALTER COLUMN password_hash SET NOT NULL`,
	} {
		_, err := conn.Exec(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	var adminID string
	require.NoError(t, conn.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, role, display_name, show_uptime_display)
		 VALUES ('keep18@example.com', 'hash18', 'admin', 'Keep', TRUE)
		 RETURNING id`).Scan(&adminID))

	_, err = store.CreateKid(ctx, "Pre", "blue")
	require.Error(t, err, "precondition: before 0018 a kid cannot be stored")

	// First run — the upgrade.
	require.NoError(t, store.Migrate(ctx))

	u, err := store.UserByID(ctx, adminID)
	require.NoError(t, err)
	assert.Equal(t, "keep18@example.com", u.Email, "existing adults are untouched")
	assert.Equal(t, "hash18", u.PasswordHash)
	assert.Equal(t, "admin", u.Role)
	assert.Equal(t, "Keep", u.DisplayName)
	assert.Equal(t, "", u.AvatarColor, "existing adults have no avatar color")

	kid, err := store.CreateKid(ctx, "Sam", "green")
	require.NoError(t, err)
	svc, err := store.CreateService(ctx, adminID, storage.Service{Slug: "m18", Name: "M18", URL: "https://m18.example.com"})
	require.NoError(t, err)
	require.NoError(t, store.SetKidServices(ctx, kid.ID, []string{svc.ID}))

	// Every later boot — a no-op that keeps kids, their allow-lists and adults.
	require.NoError(t, store.Migrate(ctx))
	require.NoError(t, store.Migrate(ctx))

	got, err := store.UserByID(ctx, kid.ID)
	require.NoError(t, err)
	assert.Equal(t, kid, got)
	ids, err := store.KidServiceIDs(ctx, kid.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{svc.ID}, ids, "re-running migrations keeps allow-lists")

	var nullable string
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT is_nullable FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'email'`).Scan(&nullable))
	assert.Equal(t, "YES", nullable)
	_, err = conn.Exec(ctx, `INSERT INTO users (email, password_hash, role, show_uptime_display) VALUES (NULL, 'x', 'user', TRUE)`)
	assertCheck(t, err, "users_kid_credentials_check", "after any number of boots, adults still need an email")

	// Leave the DB clean for the rest of the package.
	_, err = conn.Exec(ctx, `TRUNCATE categories, users, system_settings CASCADE`)
	require.NoError(t, err)
}
