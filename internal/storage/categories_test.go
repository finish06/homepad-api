package storage_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// v4 app-categories — storage layer. These drive the category model + the
// nullable services.category_id FK directly against the test Postgres
// (DATABASE_URL); they are skipped when it is unset, like the rest of the
// storage integration tests.
//
// The integration DB is shared with the api test binary, whose truncate wipes
// the categories/services tables wholesale — so these tests, which create rows
// and then assert they persist (duplicate-name collision, reorder, delete),
// cannot run concurrently with that package. The suite serializes integration
// package binaries with `go test -p 1` (CI + Makefile). To stay robust even so,
// these tests never assert on global counts or absolute sort_index: they create
// uniquely-named rows and assert only on their own rows (by id) and on relative
// ordering among them.

var catSeq int

// uniqueName returns a category name no other test (here or in the api package)
// uses, so create/rename never collide with a concurrently-seeded row. Storage
// tests run serially within their package, so a plain counter is enough.
func uniqueName(t *testing.T) string {
	t.Helper()
	catSeq++
	return fmt.Sprintf("stg-%s-%d", t.Name(), catSeq)
}

func openStore(t *testing.T) (*storage.Store, context.Context) {
	t.Helper()
	dsn := requireDatabaseURL(t)
	ctx := context.Background()
	store, err := storage.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	require.NoError(t, store.Migrate(ctx))
	return store, ctx
}

func TestCreateCategory_AppendsAfterExisting(t *testing.T) {
	store, ctx := openStore(t)

	a, err := store.CreateCategory(ctx, uniqueName(t))
	require.NoError(t, err)
	b, err := store.CreateCategory(ctx, uniqueName(t))
	require.NoError(t, err)

	assert.Greater(t, b.SortIndex, a.SortIndex,
		"a later-created category appends after an earlier one (sort_index = max+1)")

	// Both are present and a precedes b among our own rows.
	idx := indexCategories(t, store, ctx, a.ID, b.ID)
	assert.Less(t, idx[a.ID], idx[b.ID], "ListCategories returns our rows in sort_index order")
}

func TestCreateCategory_DuplicateName_ErrNameTaken(t *testing.T) {
	store, ctx := openStore(t)

	name := uniqueName(t)
	_, err := store.CreateCategory(ctx, name)
	require.NoError(t, err)

	_, err = store.CreateCategory(ctx, name)
	assert.ErrorIs(t, err, storage.ErrNameTaken, "duplicate name must be ErrNameTaken")
}

func TestRenameCategory(t *testing.T) {
	store, ctx := openStore(t)

	media, err := store.CreateCategory(ctx, uniqueName(t))
	require.NoError(t, err)
	other, err := store.CreateCategory(ctx, uniqueName(t))
	require.NoError(t, err)

	newName := uniqueName(t)
	renamed, err := store.RenameCategory(ctx, media.ID, newName)
	require.NoError(t, err)
	assert.Equal(t, newName, renamed.Name)
	assert.Equal(t, media.SortIndex, renamed.SortIndex, "rename leaves order untouched")

	_, err = store.RenameCategory(ctx, media.ID, other.Name)
	assert.ErrorIs(t, err, storage.ErrNameTaken, "renaming onto an existing name must collide")

	_, err = store.RenameCategory(ctx, "00000000-0000-0000-0000-000000000000", uniqueName(t))
	assert.ErrorIs(t, err, storage.ErrNotFound, "unknown id must be ErrNotFound")
}

func TestSetCategoryOrder_Reindexes(t *testing.T) {
	store, ctx := openStore(t)

	a, _ := store.CreateCategory(ctx, uniqueName(t))
	b, _ := store.CreateCategory(ctx, uniqueName(t))
	c, _ := store.CreateCategory(ctx, uniqueName(t))

	require.NoError(t, store.SetCategoryOrder(ctx, []string{c.ID, a.ID, b.ID}))

	// Among our three rows, the new relative order is c, a, b.
	idx := indexCategories(t, store, ctx, a.ID, b.ID, c.ID)
	assert.Less(t, idx[c.ID], idx[a.ID])
	assert.Less(t, idx[a.ID], idx[b.ID])
}

func TestDeleteCategory_SetsServicesNull_AndIdempotent(t *testing.T) {
	store, ctx := openStore(t)

	cat, err := store.CreateCategory(ctx, uniqueName(t))
	require.NoError(t, err)
	svc, err := store.CreateService(ctx, storage.Service{
		Slug: uniqueName(t), Name: "Jellyfin", URL: "https://jellyfin.test", Icon: "jellyfin",
	})
	require.NoError(t, err)

	updated, err := store.UpdateService(ctx, svc.ID, storage.ServiceUpdate{
		SetCategory: true, CategoryID: &cat.ID,
	})
	require.NoError(t, err)
	require.NotNil(t, updated.CategoryID)
	assert.Equal(t, cat.ID, *updated.CategoryID)

	require.NoError(t, store.DeleteCategory(ctx, cat.ID))

	// The service survives, now Uncategorized (FK SET NULL). Look it up by id —
	// the shared DB holds other packages' rows too.
	got := findStored(t, store, ctx, svc.ID)
	assert.Nil(t, got.CategoryID, "service falls back to Uncategorized; it is not deleted")

	// Deleting again is a no-op (idempotent), and so is a malformed id.
	assert.NoError(t, store.DeleteCategory(ctx, cat.ID))
	assert.NoError(t, store.DeleteCategory(ctx, "not-a-uuid"))
}

func TestUpdateService_CategoryThreeState(t *testing.T) {
	store, ctx := openStore(t)

	cat, err := store.CreateCategory(ctx, uniqueName(t))
	require.NoError(t, err)
	svc, err := store.CreateService(ctx, storage.Service{
		Slug: uniqueName(t), Name: "Jellyfin", URL: "https://jellyfin.test", Icon: "jellyfin",
	})
	require.NoError(t, err)
	require.Nil(t, svc.CategoryID, "a new service is Uncategorized")

	// set
	set, err := store.UpdateService(ctx, svc.ID, storage.ServiceUpdate{SetCategory: true, CategoryID: &cat.ID})
	require.NoError(t, err)
	require.NotNil(t, set.CategoryID)
	assert.Equal(t, cat.ID, *set.CategoryID)
	require.NotNil(t, set.CategoryName)
	assert.Equal(t, cat.Name, *set.CategoryName, "categoryName is denormalized from the joined category")

	// absent (SetCategory false) leaves the assignment unchanged
	unchanged, err := store.UpdateService(ctx, svc.ID, storage.ServiceUpdate{Name: ptr("Jellyfin 2")})
	require.NoError(t, err)
	require.NotNil(t, unchanged.CategoryID, "absent categoryId must leave the assignment alone")
	assert.Equal(t, cat.ID, *unchanged.CategoryID)

	// clear to NULL
	cleared, err := store.UpdateService(ctx, svc.ID, storage.ServiceUpdate{SetCategory: true, CategoryID: nil})
	require.NoError(t, err)
	assert.Nil(t, cleared.CategoryID, "clearing sets category_id NULL")
	assert.Nil(t, cleared.CategoryName)
}

func TestUpdateService_UnknownCategory_ErrCategoryNotFound(t *testing.T) {
	store, ctx := openStore(t)

	svc, err := store.CreateService(ctx, storage.Service{
		Slug: uniqueName(t), Name: "Jellyfin", URL: "https://jellyfin.test", Icon: "jellyfin",
	})
	require.NoError(t, err)

	bogus := "11111111-1111-1111-1111-111111111111"
	_, err = store.UpdateService(ctx, svc.ID, storage.ServiceUpdate{SetCategory: true, CategoryID: &bogus})
	assert.True(t, errors.Is(err, storage.ErrCategoryNotFound),
		"assigning an unknown category must be ErrCategoryNotFound, got %v", err)

	// A malformed category UUID is likewise a category-not-found, not a 500.
	_, err = store.UpdateService(ctx, svc.ID, storage.ServiceUpdate{SetCategory: true, CategoryID: ptr("not-a-uuid")})
	assert.True(t, errors.Is(err, storage.ErrCategoryNotFound),
		"malformed category id must be ErrCategoryNotFound, got %v", err)

	// The service is untouched by the failed assignment.
	got := findStored(t, store, ctx, svc.ID)
	assert.Nil(t, got.CategoryID, "a rejected assignment must leave the service Uncategorized")
}

func ptr(s string) *string { return &s }

// indexCategories returns, for each requested id, its position in the global
// sort_index ordering, so tests can assert relative order among their own rows
// without depending on other packages' categories.
func indexCategories(t *testing.T, store *storage.Store, ctx context.Context, ids ...string) map[string]int {
	t.Helper()
	cats, err := store.ListCategories(ctx)
	require.NoError(t, err)
	pos := make(map[string]int)
	for i, c := range cats {
		pos[c.ID] = i
	}
	out := make(map[string]int, len(ids))
	for _, id := range ids {
		p, ok := pos[id]
		require.True(t, ok, "category %s missing from ListCategories", id)
		out[id] = p
	}
	return out
}

// findStored returns the service with the given id, failing if absent. The
// integration DB is shared across packages, so tests assert on their own rows
// by id rather than on the catalog's total contents.
func findStored(t *testing.T, store *storage.Store, ctx context.Context, id string) storage.Service {
	t.Helper()
	svcs, err := store.ListServices(ctx, "00000000-0000-0000-0000-000000000000")
	require.NoError(t, err)
	for _, sv := range svcs {
		if sv.ID == id {
			return sv
		}
	}
	t.Fatalf("service %s not found", id)
	return storage.Service{}
}
