package storage_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// Migration 0016 — cap6 v2: "show uptime display" becomes a per-user column,
// seeded once from the old global system_settings value.
//
// The seeding UPDATE is the dangerous part. Migrations run on EVERY boot, so an
// unguarded seed would reset every user's choice back to the global value on
// every API restart — presenting as "the setting randomly forgets itself"
// (AC-021). Same exercise shape as migration_0013_test: put the table back into
// its PRE-0016 form, migrate, assert the seed, then migrate AGAIN and assert the
// user's own choice survived.
func TestMigration0016_SeedsFromGlobalAndSeedsOnlyOnce(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test (needs Postgres)")
	}
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

	// PRE-0016 shape: no per-user column at all.
	_, err = conn.Exec(ctx, `ALTER TABLE users DROP COLUMN IF EXISTS show_uptime_display`)
	require.NoError(t, err)

	// The admin had turned the global display OFF. AC-020 says nobody's view may
	// change, so every existing user must come out of the migration OFF too.
	_, err = conn.Exec(ctx,
		`INSERT INTO system_settings (id, show_uptime_display) VALUES (1, FALSE)
		 ON CONFLICT (id) DO UPDATE SET show_uptime_display = FALSE`)
	require.NoError(t, err)

	var keepID, flipID string
	require.NoError(t, conn.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, role) VALUES ('keep16@example.com','x','admin') RETURNING id`).Scan(&keepID))
	require.NoError(t, conn.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, role) VALUES ('flip16@example.com','x','user') RETURNING id`).Scan(&flipID))

	// First run — the seed.
	require.NoError(t, store.Migrate(ctx))

	var keep, flip bool
	require.NoError(t, conn.QueryRow(ctx, `SELECT show_uptime_display FROM users WHERE id=$1`, keepID).Scan(&keep))
	require.NoError(t, conn.QueryRow(ctx, `SELECT show_uptime_display FROM users WHERE id=$1`, flipID).Scan(&flip))
	assert.False(t, keep, "AC-020: an admin who had it OFF globally must not find it back ON")
	assert.False(t, flip, "AC-020: every existing user inherits the global value")

	// One user then makes their own choice, the opposite of the global default.
	_, err = conn.Exec(ctx, `UPDATE users SET show_uptime_display = TRUE WHERE id=$1`, flipID)
	require.NoError(t, err)

	// Second run — this is the regression that matters. An unguarded seed would
	// stamp the global FALSE back over that choice, on this and every restart.
	require.NoError(t, store.Migrate(ctx))

	require.NoError(t, conn.QueryRow(ctx, `SELECT show_uptime_display FROM users WHERE id=$1`, flipID).Scan(&flip))
	assert.True(t, flip, "AC-021: the seed must run exactly once — a user's own choice must survive a restart")

	require.NoError(t, conn.QueryRow(ctx, `SELECT show_uptime_display FROM users WHERE id=$1`, keepID).Scan(&keep))
	assert.False(t, keep, "AC-021: an untouched user stays as seeded")
}

// AC-026/AC-027a — a NEW account inherits the admin default, tested in the
// direction that can actually fail. With the default ON a stale column default
// would pass this on its own and prove nothing; with it OFF, only a CreateUser
// that genuinely reads system_settings can succeed.
func TestCreateUser_InheritsAdminDefaultWhenOff(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test (needs Postgres)")
	}
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

	_, err = conn.Exec(ctx,
		`INSERT INTO system_settings (id, show_uptime_display) VALUES (1, FALSE)
		 ON CONFLICT (id) DO UPDATE SET show_uptime_display = FALSE`)
	require.NoError(t, err)

	u, err := store.CreateUser(ctx, "new16@example.com", "hash", "user")
	require.NoError(t, err)
	assert.False(t, u.ShowUptimeDisplay,
		"AC-026: a new account must inherit the admin default (OFF), not the column default")

	// AC-027 — flipping the admin default must not reach back into that account.
	_, err = conn.Exec(ctx, `UPDATE system_settings SET show_uptime_display = TRUE WHERE id = 1`)
	require.NoError(t, err)
	var still bool
	require.NoError(t, conn.QueryRow(ctx, `SELECT show_uptime_display FROM users WHERE id=$1`, u.ID).Scan(&still))
	assert.False(t, still, "AC-027: changing the default must not alter an existing account")
}
