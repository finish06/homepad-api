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

// v4 app-categories — storage layer, now PER-USER (v9). These drive the
// category model + the nullable services.category_id FK directly against the
// test Postgres (DATABASE_URL); they are skipped when it is unset, like the
// rest of the storage integration tests.
//
// v9: categories + services are owned by a user_id (NOT NULL). Each test seeds
// its OWN user and threads that id through, so create/rename/reorder/delete and
// the list reads scope to that user — which also makes these tests fully
// isolated from rows other packages leave in the shared DB.

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

// seedStoreUser creates a unique user and returns its id, so per-user storage
// calls have a real owner (FK + NOT NULL).
func seedStoreUser(t *testing.T, store *storage.Store, ctx context.Context) string {
	t.Helper()
	catSeq++
	u, err := store.CreateUser(ctx, fmt.Sprintf("stg-%s-%d@example.com", t.Name(), catSeq), "x", "user")
	require.NoError(t, err)
	return u.ID
}

func TestCreateCategory_AppendsAfterExisting(t *testing.T) {
	store, ctx := openStore(t)
	uid := seedStoreUser(t, store, ctx)

	a, err := store.CreateCategory(ctx, uid, uniqueName(t))
	require.NoError(t, err)
	b, err := store.CreateCategory(ctx, uid, uniqueName(t))
	require.NoError(t, err)

	assert.Greater(t, b.SortIndex, a.SortIndex,
		"a later-created category appends after an earlier one (sort_index = max+1)")

	idx := indexCategories(t, store, ctx, uid, a.ID, b.ID)
	assert.Less(t, idx[a.ID], idx[b.ID], "ListCategories returns our rows in sort_index order")
}

func TestCreateCategory_DuplicateName_ErrNameTaken(t *testing.T) {
	store, ctx := openStore(t)
	uid := seedStoreUser(t, store, ctx)

	name := uniqueName(t)
	_, err := store.CreateCategory(ctx, uid, name)
	require.NoError(t, err)

	_, err = store.CreateCategory(ctx, uid, name)
	assert.ErrorIs(t, err, storage.ErrNameTaken, "duplicate name (same user) must be ErrNameTaken")

	// D3 — a DIFFERENT user may reuse the same category name.
	other := seedStoreUser(t, store, ctx)
	_, err = store.CreateCategory(ctx, other, name)
	assert.NoError(t, err, "another user may reuse a category name (per-user uniqueness)")
}

func TestRenameCategory(t *testing.T) {
	store, ctx := openStore(t)
	uid := seedStoreUser(t, store, ctx)

	media, err := store.CreateCategory(ctx, uid, uniqueName(t))
	require.NoError(t, err)
	other, err := store.CreateCategory(ctx, uid, uniqueName(t))
	require.NoError(t, err)

	newName := uniqueName(t)
	renamed, err := store.RenameCategory(ctx, media.ID, uid, newName)
	require.NoError(t, err)
	assert.Equal(t, newName, renamed.Name)
	assert.Equal(t, media.SortIndex, renamed.SortIndex, "rename leaves order untouched")

	_, err = store.RenameCategory(ctx, media.ID, uid, other.Name)
	assert.ErrorIs(t, err, storage.ErrNameTaken, "renaming onto an existing name must collide")

	_, err = store.RenameCategory(ctx, "00000000-0000-0000-0000-000000000000", uid, uniqueName(t))
	assert.ErrorIs(t, err, storage.ErrNotFound, "unknown id must be ErrNotFound")
}

func TestSetCategoryOrder_Reindexes(t *testing.T) {
	store, ctx := openStore(t)
	uid := seedStoreUser(t, store, ctx)

	a, _ := store.CreateCategory(ctx, uid, uniqueName(t))
	b, _ := store.CreateCategory(ctx, uid, uniqueName(t))
	c, _ := store.CreateCategory(ctx, uid, uniqueName(t))

	require.NoError(t, store.SetCategoryOrder(ctx, uid, []string{c.ID, a.ID, b.ID}))

	idx := indexCategories(t, store, ctx, uid, a.ID, b.ID, c.ID)
	assert.Less(t, idx[c.ID], idx[a.ID])
	assert.Less(t, idx[a.ID], idx[b.ID])
}

func TestDeleteCategory_SetsServicesNull_AndIdempotent(t *testing.T) {
	store, ctx := openStore(t)
	uid := seedStoreUser(t, store, ctx)

	cat, err := store.CreateCategory(ctx, uid, uniqueName(t))
	require.NoError(t, err)
	svc, err := store.CreateService(ctx, uid, storage.Service{
		Slug: uniqueName(t), Name: "Jellyfin", URL: "https://jellyfin.test", Icon: "jellyfin",
	})
	require.NoError(t, err)

	updated, err := store.UpdateService(ctx, svc.ID, uid, storage.ServiceUpdate{
		SetCategory: true, CategoryID: &cat.ID,
	})
	require.NoError(t, err)
	require.NotNil(t, updated.CategoryID)
	assert.Equal(t, cat.ID, *updated.CategoryID)

	require.NoError(t, store.DeleteCategory(ctx, cat.ID, uid))

	got := findStored(t, store, ctx, uid, svc.ID)
	assert.Nil(t, got.CategoryID, "service falls back to Uncategorized; it is not deleted")

	// Deleting again is a no-op (idempotent), and so is a malformed id.
	assert.NoError(t, store.DeleteCategory(ctx, cat.ID, uid))
	assert.NoError(t, store.DeleteCategory(ctx, "not-a-uuid", uid))
}

func TestUpdateService_CategoryThreeState(t *testing.T) {
	store, ctx := openStore(t)
	uid := seedStoreUser(t, store, ctx)

	cat, err := store.CreateCategory(ctx, uid, uniqueName(t))
	require.NoError(t, err)
	svc, err := store.CreateService(ctx, uid, storage.Service{
		Slug: uniqueName(t), Name: "Jellyfin", URL: "https://jellyfin.test", Icon: "jellyfin",
	})
	require.NoError(t, err)
	require.Nil(t, svc.CategoryID, "a new service is Uncategorized")

	// set
	set, err := store.UpdateService(ctx, svc.ID, uid, storage.ServiceUpdate{SetCategory: true, CategoryID: &cat.ID})
	require.NoError(t, err)
	require.NotNil(t, set.CategoryID)
	assert.Equal(t, cat.ID, *set.CategoryID)
	require.NotNil(t, set.CategoryName)
	assert.Equal(t, cat.Name, *set.CategoryName, "categoryName is denormalized from the joined category")

	// absent (SetCategory false) leaves the assignment unchanged
	unchanged, err := store.UpdateService(ctx, svc.ID, uid, storage.ServiceUpdate{Name: ptr("Jellyfin 2")})
	require.NoError(t, err)
	require.NotNil(t, unchanged.CategoryID, "absent categoryId must leave the assignment alone")
	assert.Equal(t, cat.ID, *unchanged.CategoryID)

	// clear to NULL
	cleared, err := store.UpdateService(ctx, svc.ID, uid, storage.ServiceUpdate{SetCategory: true, CategoryID: nil})
	require.NoError(t, err)
	assert.Nil(t, cleared.CategoryID, "clearing sets category_id NULL")
	assert.Nil(t, cleared.CategoryName)
}

func TestUpdateService_UnknownCategory_ErrCategoryNotFound(t *testing.T) {
	store, ctx := openStore(t)
	uid := seedStoreUser(t, store, ctx)

	svc, err := store.CreateService(ctx, uid, storage.Service{
		Slug: uniqueName(t), Name: "Jellyfin", URL: "https://jellyfin.test", Icon: "jellyfin",
	})
	require.NoError(t, err)

	bogus := "11111111-1111-1111-1111-111111111111"
	_, err = store.UpdateService(ctx, svc.ID, uid, storage.ServiceUpdate{SetCategory: true, CategoryID: &bogus})
	assert.True(t, errors.Is(err, storage.ErrCategoryNotFound),
		"assigning an unknown category must be ErrCategoryNotFound, got %v", err)

	// A malformed category UUID is likewise a category-not-found, not a 500.
	_, err = store.UpdateService(ctx, svc.ID, uid, storage.ServiceUpdate{SetCategory: true, CategoryID: ptr("not-a-uuid")})
	assert.True(t, errors.Is(err, storage.ErrCategoryNotFound),
		"malformed category id must be ErrCategoryNotFound, got %v", err)

	// A7 — another user's category id is also rejected (ErrCategoryNotFound).
	other := seedStoreUser(t, store, ctx)
	foreignCat, err := store.CreateCategory(ctx, other, uniqueName(t))
	require.NoError(t, err)
	_, err = store.UpdateService(ctx, svc.ID, uid, storage.ServiceUpdate{SetCategory: true, CategoryID: &foreignCat.ID})
	assert.True(t, errors.Is(err, storage.ErrCategoryNotFound),
		"assigning another user's category must be ErrCategoryNotFound, got %v", err)

	// The service is untouched by the failed assignment.
	got := findStored(t, store, ctx, uid, svc.ID)
	assert.Nil(t, got.CategoryID, "a rejected assignment must leave the service Uncategorized")
}

func ptr(s string) *string { return &s }

// indexCategories returns, for each requested id, its position in uid's
// sort_index ordering.
func indexCategories(t *testing.T, store *storage.Store, ctx context.Context, uid string, ids ...string) map[string]int {
	t.Helper()
	cats, err := store.ListCategories(ctx, uid)
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

// findStored returns uid's service with the given id, failing if absent.
func findStored(t *testing.T, store *storage.Store, ctx context.Context, uid, id string) storage.Service {
	t.Helper()
	svcs, err := store.ListServices(ctx, uid)
	require.NoError(t, err)
	for _, sv := range svcs {
		if sv.ID == id {
			return sv
		}
	}
	t.Fatalf("service %s not found", id)
	return storage.Service{}
}
