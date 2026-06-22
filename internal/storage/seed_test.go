package storage_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// zeroUUID is a syntactically-valid UUID that owns no services, so ListLibrary's
// per-user `added` subquery is always false — we only care about the offers.
const zeroUUID = "00000000-0000-0000-0000-000000000000"

func resetLibrary(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close(ctx)
	// ON DELETE SET NULL on services.source_library_id means a plain DELETE is
	// safe — it does not touch any service rows.
	_, err = conn.Exec(ctx, `DELETE FROM library_apps`)
	require.NoError(t, err)
}

// Auto-seed-on-empty: homepad-db is ephemeral (emptyDir) in prod, so a restart
// wipes the catalog. On boot, an empty App Library must self-seed from the
// committed catalog seed so a fresh DB is never empty again.
func TestSeedLibraryIfEmpty_SeedsFreshDB(t *testing.T) {
	dsn := requireDatabaseURL(t)
	ctx := context.Background()
	store, err := storage.Open(ctx, dsn)
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.Migrate(ctx))

	resetLibrary(t, ctx, dsn)

	n, err := store.SeedLibraryIfEmpty(ctx)
	require.NoError(t, err)
	require.Greater(t, n, 0, "a fresh DB must self-seed the App Library from the committed seed")

	offers, err := store.ListLibrary(ctx, zeroUUID)
	require.NoError(t, err)
	require.Equal(t, n, len(offers), "every seeded offer must be present in the library")

	var names, cats []string
	for _, o := range offers {
		names = append(names, o.Name)
		cats = append(cats, o.SuggestedCategory)
	}
	require.Contains(t, names, "PocketID", "the committed prod catalog must be seeded")
	require.Contains(t, cats, "External", "gatus group must map to a curated suggested category")
}

// Idempotency: once any offer exists the seed is a no-op, so admin curation is
// never clobbered and racing replicas never double-seed.
func TestSeedLibraryIfEmpty_NoopWhenPresent(t *testing.T) {
	dsn := requireDatabaseURL(t)
	ctx := context.Background()
	store, err := storage.Open(ctx, dsn)
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.Migrate(ctx))

	resetLibrary(t, ctx, dsn)
	if _, err := store.SeedLibraryIfEmpty(ctx); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	before, err := store.ListLibrary(ctx, zeroUUID)
	require.NoError(t, err)

	n2, err := store.SeedLibraryIfEmpty(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n2, "second seed must insert nothing")

	after, err := store.ListLibrary(ctx, zeroUUID)
	require.NoError(t, err)
	require.Equal(t, len(before), len(after), "re-seeding must not double the catalog")
}
