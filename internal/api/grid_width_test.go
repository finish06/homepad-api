package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-app-grid §10.4 (2026-09-12, OQ-3) — category.grid_width is a 12-COLUMN
// SPAN: 3 (quarter), 4 (third), 6 (half) or 12 (full). It replaces the 1–8
// tile-count model (§3B + A1); migration 0013 remaps stored values and keeps
// the old ones in grid_width_legacy. Persisted, read on GET /api/categories,
// written via PATCH /api/categories/{id} {gridWidth}, owner-scoped.

type gwCat struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	GridWidth int    `json:"gridWidth"`
}

func getGWCats(t *testing.T, baseURL, token string) map[string]gwCat {
	t.Helper()
	resp := doJSON(t, http.MethodGet, baseURL+"/api/categories", token, nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var payload struct {
		Categories []gwCat `json:"categories"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	out := map[string]gwCat{}
	for _, c := range payload.Categories {
		out[c.ID] = c
	}
	return out
}

// §10.4 — a newly created box defaults to a HALF-width span (6): the same share
// of the row the old default 3-of-6 had.
func TestCreateCategory_DefaultsToHalfSpan(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	c := createCategory(t, s.URL, "admin-session", "Media")
	got := getGWCats(t, s.URL, "admin-session")[c.ID]
	assert.Equal(t, 6, got.GridWidth, "new category must default to span 6 (half)")
}

// AC-018 — an admin changes a box span; it persists across a fresh read.
func TestPatchCategoryGridWidth_Persists(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	c := createCategory(t, s.URL, "admin-session", "Development")

	resp := doJSON(t, http.MethodPatch, s.URL+"/api/categories/"+c.ID, "admin-session",
		map[string]any{"gridWidth": 4})
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "PATCH gridWidth must return 200")

	got := getGWCats(t, s.URL, "admin-session")[c.ID]
	assert.Equal(t, 4, got.GridWidth, "gridWidth must persist across a re-read")
}

// §10.4 — every legal span is accepted and persisted (API validator AND DB CHECK).
func TestPatchCategoryGridWidth_AcceptsEverySpan(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	c := createCategory(t, s.URL, "admin-session", "Wide")

	for _, w := range []int{3, 4, 6, 12} {
		resp := doJSON(t, http.MethodPatch, s.URL+"/api/categories/"+c.ID, "admin-session",
			map[string]any{"gridWidth": w})
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, "span %d must be accepted", w)

		got := getGWCats(t, s.URL, "admin-session")[c.ID]
		assert.Equal(t, w, got.GridWidth, "span %d must persist", w)
	}
}

// §10.4 — anything that is not one of the four spans is rejected (400) and
// nothing changes. That includes the OLD tile counts that have no span twin
// (1, 2, 5, 7, 8): the API no longer speaks the 1–8 model at all.
func TestPatchCategoryGridWidth_RejectsNonSpans(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	c := createCategory(t, s.URL, "admin-session", "Infra")

	for _, bad := range []int{0, -1, 1, 2, 5, 7, 8, 9, 11, 13} {
		resp := doJSON(t, http.MethodPatch, s.URL+"/api/categories/"+c.ID, "admin-session",
			map[string]any{"gridWidth": bad})
		resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "gridWidth %d must be rejected", bad)
	}

	got := getGWCats(t, s.URL, "admin-session")[c.ID]
	assert.Equal(t, 6, got.GridWidth, "a rejected width must leave the stored value unchanged")
}

// A gridWidth-only PATCH must not require or clobber the name, and a name-only
// PATCH must still work (the endpoint stays backward-compatible).
func TestPatchCategory_NameOnly_LeavesGridWidth(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	c := createCategory(t, s.URL, "admin-session", "Media")
	// set a non-default span first
	doJSON(t, http.MethodPatch, s.URL+"/api/categories/"+c.ID, "admin-session",
		map[string]any{"gridWidth": 12}).Body.Close()

	resp := doJSON(t, http.MethodPatch, s.URL+"/api/categories/"+c.ID, "admin-session",
		map[string]any{"name": "Movies"})
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := getGWCats(t, s.URL, "admin-session")[c.ID]
	assert.Equal(t, "Movies", got.Name, "name-only PATCH renames")
	assert.Equal(t, 12, got.GridWidth, "name-only PATCH must not reset gridWidth")
}
