package api_test

import (
	"net/http"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-245-224 — shared catalog model. Reads (categories, services) return the
// single admin-managed set to EVERY authenticated user (#245: non-admins used
// to get an empty grid); writes are admin-only 403 (#224: any user used to be
// able to mutate the catalog). This supersedes the v9 per-user catalog model
// (SPEC-app-grid §3C / AC-025) and Invariant 2's per-user READ/WRITE isolation
// for the catalog — per-user favorites and collapse state are unchanged.
//
// Fixtures (testsupport.NewServer): admin-session → the first admin (the shared
// catalog owner, per migration 0007); non-admin-session → a plain user. Both are
// seeded their own gitea/grafana rows, so a per-user read would return each
// their OWN ids — proving the shared read means asserting non-admin sees the
// ADMIN's ids.

func serviceIDs(items []struct {
	ID       string `json:"id"`
	Favorite bool   `json:"favorite"`
}) []string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		out = append(out, s.ID)
	}
	sort.Strings(out)
	return out
}

// AC-001 / AC-003 — a non-admin sees the same categories as the admin (the
// shared, admin-managed set), not an empty grid.
func TestSharedCatalog_NonAdminSeesAdminCategories(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	// Admin curates the shared catalog.
	createCategory(t, s.URL, "admin-session", "Media")
	createCategory(t, s.URL, "admin-session", "Tools")

	adminCats := getCategories(t, s.URL, "admin-session")
	userCats := getCategories(t, s.URL, "non-admin-session")

	require.Len(t, adminCats, 2, "admin created two categories")
	// AC-001: the non-admin's grid is NOT empty — it mirrors the admin's.
	require.Len(t, userCats, 2, "non-admin must see the shared categories, not an empty grid")
	// AC-003: identical list (same ids, same order) regardless of caller.
	assert.Equal(t, adminCats, userCats, "GET /api/categories is identical for every authenticated user")
}

// AC-002 / AC-004 — a non-admin sees the same service tiles as the admin (same
// row ids), and the favorite field reflects the CALLING user (per-user).
func TestSharedCatalog_NonAdminSeesAdminServices(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	adminSvcs := listServices(t, s.URL, "admin-session")
	userSvcs := listServices(t, s.URL, "non-admin-session")

	require.NotEmpty(t, adminSvcs, "admin has the seeded shared services")
	// AC-002/AC-004: same service ROWS (ids), not each user's own copies.
	assert.Equal(t, serviceIDs(adminSvcs), serviceIDs(userSvcs),
		"GET /api/services returns the same shared rows to every authenticated user")
}

// AC-004 / AC-013 / AC-014 — favorites remain per-user even on the shared set:
// the admin favoriting a shared service does NOT make it favorited for the
// non-admin.
func TestSharedCatalog_FavoritesRemainPerUser(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	// Admin owns the shared services, so the admin can favorite one.
	svcs := listServices(t, s.URL, "admin-session")
	require.NotEmpty(t, svcs)
	target := svcs[0].ID

	resp := doJSON(t, http.MethodPost, s.URL+"/api/favorites/"+target, "admin-session", nil)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	// Admin now sees it favorited; the non-admin sees the SAME shared row but
	// favorite=false (their own favorite set is independent).
	var adminFav, userFav bool
	var userHasRow bool
	for _, sv := range listServices(t, s.URL, "admin-session") {
		if sv.ID == target {
			adminFav = sv.Favorite
		}
	}
	for _, sv := range listServices(t, s.URL, "non-admin-session") {
		if sv.ID == target {
			userHasRow = true
			userFav = sv.Favorite
		}
	}
	assert.True(t, adminFav, "AC-014: favorite reflects the calling (admin) user")
	require.True(t, userHasRow, "AC-002: non-admin sees the same shared service row")
	assert.False(t, userFav, "AC-013: favorites are per-user — admin's favorite must not leak to the non-admin")
}

// AC-005..AC-011 — every catalog write is admin-only: a non-admin gets 403.
func TestSharedCatalog_NonAdminWritesForbidden(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	// Real ids from the shared set so the gate is reached before any not-found.
	cat := createCategory(t, s.URL, "admin-session", "Media")
	svcID := listServices(t, s.URL, "admin-session")[0].ID

	writes := []struct {
		ac, name, method, url string
		body                  any
	}{
		{"AC-005", "POST /api/categories", http.MethodPost, "/api/categories", map[string]any{"name": "X"}},
		{"AC-006", "PATCH /api/categories/{id}", http.MethodPatch, "/api/categories/" + cat.ID, map[string]any{"name": "X"}},
		{"AC-007", "DELETE /api/categories/{id}", http.MethodDelete, "/api/categories/" + cat.ID, nil},
		{"AC-008", "PUT /api/categories/order", http.MethodPut, "/api/categories/order", map[string]any{"order": []string{cat.ID}}},
		{"AC-009", "POST /api/services", http.MethodPost, "/api/services", map[string]any{"slug": "x", "name": "X", "url": "https://x.example.com"}},
		{"AC-010", "PATCH /api/services/{id}", http.MethodPatch, "/api/services/" + svcID, map[string]any{"name": "X"}},
		{"AC-011", "DELETE /api/services/{id}", http.MethodDelete, "/api/services/" + svcID, nil},
	}
	for _, wtc := range writes {
		t.Run(wtc.ac+" "+wtc.name, func(t *testing.T) {
			r := doJSON(t, wtc.method, s.URL+wtc.url, "non-admin-session", wtc.body)
			defer r.Body.Close()
			assert.Equal(t, http.StatusForbidden, r.StatusCode,
				"%s: a non-admin catalog write must be 403 Forbidden", wtc.ac)
		})
	}

	// Icon upload/delete are catalog writes too (AC-011 family).
	t.Run("AC-011 PUT /api/services/{id}/icon/light", func(t *testing.T) {
		r := putIcon(t, s.URL, "non-admin-session", svcID, "light", pngBytes(t, 32, 32))
		defer r.Body.Close()
		assert.Equal(t, http.StatusForbidden, r.StatusCode, "non-admin icon upload must be 403")
	})
	t.Run("AC-011 DELETE /api/services/{id}/icon/light", func(t *testing.T) {
		r := doJSON(t, http.MethodDelete, s.URL+"/api/services/"+svcID+"/icon/light", "non-admin-session", nil)
		defer r.Body.Close()
		assert.Equal(t, http.StatusForbidden, r.StatusCode, "non-admin icon delete must be 403")
	})
}

// AC-012 — an admin can still create, rename, reorder, and delete categories and
// services (the gate lets admins through unchanged).
func TestSharedCatalog_AdminWritesStillWork(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	cat := createCategory(t, s.URL, "admin-session", "Media") // 201 or the helper fails

	rename := doJSON(t, http.MethodPatch, s.URL+"/api/categories/"+cat.ID, "admin-session", map[string]any{"name": "Movies"})
	rename.Body.Close()
	assert.Equal(t, http.StatusOK, rename.StatusCode, "admin rename category → 200")

	order := doJSON(t, http.MethodPut, s.URL+"/api/categories/order", "admin-session", map[string]any{"order": []string{cat.ID}})
	order.Body.Close()
	assert.Equal(t, http.StatusNoContent, order.StatusCode, "admin reorder → 204")

	del := doJSON(t, http.MethodDelete, s.URL+"/api/categories/"+cat.ID, "admin-session", nil)
	del.Body.Close()
	assert.Equal(t, http.StatusNoContent, del.StatusCode, "admin delete category → 204")
}
