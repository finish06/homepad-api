package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-app-grid §3B — category.grid_width: the box's App Grid width (1–6).
// Persisted (§4A DECIDED = PERSIST), read on GET /api/categories, written via
// PATCH /api/categories/{id} {gridWidth}. Owner-scoped, matching the sibling
// rename PATCH on the same endpoint. AC-018 (survives reload), AC-020 (new box
// defaults to width 3).

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

// AC-020 — a newly created box defaults to width 3.
func TestCreateCategory_DefaultsGridWidth3(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	c := createCategory(t, s.URL, "admin-session", "Media")
	got := getGWCats(t, s.URL, "admin-session")[c.ID]
	assert.Equal(t, 3, got.GridWidth, "new category must default to gridWidth 3")
}

// AC-018 — an admin changes a box width; it persists across a fresh read.
func TestPatchCategoryGridWidth_Persists(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	c := createCategory(t, s.URL, "admin-session", "Development")

	resp := doJSON(t, http.MethodPatch, s.URL+"/api/categories/"+c.ID, "admin-session",
		map[string]any{"gridWidth": 5})
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "PATCH gridWidth must return 200")

	got := getGWCats(t, s.URL, "admin-session")[c.ID]
	assert.Equal(t, 5, got.GridWidth, "gridWidth must persist across a re-read")
}

// §3B — gridWidth outside 1–6 is rejected (400) and nothing is changed.
func TestPatchCategoryGridWidth_Rejects_OutOfRange(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	c := createCategory(t, s.URL, "admin-session", "Infra")

	for _, bad := range []int{0, 7, -1} {
		resp := doJSON(t, http.MethodPatch, s.URL+"/api/categories/"+c.ID, "admin-session",
			map[string]any{"gridWidth": bad})
		resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "gridWidth %d must be rejected", bad)
	}

	got := getGWCats(t, s.URL, "admin-session")[c.ID]
	assert.Equal(t, 3, got.GridWidth, "a rejected width must leave the stored value unchanged")
}

// A gridWidth-only PATCH must not require or clobber the name, and a name-only
// PATCH must still work (the endpoint stays backward-compatible).
func TestPatchCategory_NameOnly_LeavesGridWidth(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	c := createCategory(t, s.URL, "admin-session", "Media")
	// set a non-default width first
	doJSON(t, http.MethodPatch, s.URL+"/api/categories/"+c.ID, "admin-session",
		map[string]any{"gridWidth": 6}).Body.Close()

	resp := doJSON(t, http.MethodPatch, s.URL+"/api/categories/"+c.ID, "admin-session",
		map[string]any{"name": "Movies"})
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := getGWCats(t, s.URL, "admin-session")[c.ID]
	assert.Equal(t, "Movies", got.Name, "name-only PATCH renames")
	assert.Equal(t, 6, got.GridWidth, "name-only PATCH must not reset gridWidth")
}
