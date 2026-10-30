package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/dbguard"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// AC A10 — All persistent state in Postgres; backend honors DATABASE_URL.
//
// These tests require a reachable Postgres in CI / dev (set DATABASE_URL).
// They are skipped locally when DATABASE_URL is unset so the suite stays runnable
// without docker — CI must set the env var or these ACs are not actually verified.

// requireDatabaseURL is the single accessor every storage integration test uses
// to obtain DATABASE_URL. It delegates to dbguard, which skips when the var is
// unset and FATALS (never skips, #67 AC2) on a non-test or prod-host DSN BEFORE
// any Open/Migrate/TRUNCATE/DROP — closing the gap #72 that left this package's
// destructive tests able to wipe prod the way 2026-09-13 did.
func requireDatabaseURL(t *testing.T) string {
	t.Helper()
	return dbguard.RequireTestDatabase(t)
}

func TestStorageBootsWithDatabaseURL(t *testing.T) {
	dsn := requireDatabaseURL(t)

	ctx := context.Background()
	store, err := storage.Open(ctx, dsn)
	require.NoError(t, err, "storage.Open must connect with a valid DATABASE_URL")
	defer store.Close()

	assert.Equal(t, dsn, store.DSN)
}

func TestMigrationsApplyCleanlyToFreshDB(t *testing.T) {
	dsn := requireDatabaseURL(t)

	ctx := context.Background()
	store, err := storage.Open(ctx, dsn)
	require.NoError(t, err)
	defer store.Close()

	err = store.Migrate(ctx)
	require.NoError(t, err, "Migrate must apply cleanly to a fresh DB")
}
