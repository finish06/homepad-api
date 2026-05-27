package storage_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// AC A10 — All persistent state in Postgres; backend honors DATABASE_URL.
//
// These tests require a reachable Postgres in CI / dev (set DATABASE_URL).
// They are skipped locally when DATABASE_URL is unset so the suite stays runnable
// without docker — CI must set the env var or these ACs are not actually verified.

func requireDatabaseURL(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping Postgres-backed test (AC A10 unverified in this run)")
	}
	return dsn
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
