package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// v9.2 App Library — API integration (A8, A9). Admin-gated CRUD on the shared,
// admin-curated library (offers, not assignments) + session-gated browse with
// the per-user `added` hint. Drives the real /api/library surface against the
// test Postgres via testsupport.NewServer (reuses doJSON from categories_test).

type libraryOffer struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	URL               string `json:"url"`
	Icon              string `json:"icon"`
	Description       string `json:"description"`
	SuggestedCategory string `json:"suggestedCategory"`
	SortIndex         int    `json:"sortIndex"`
	Added             bool   `json:"added"`
}

func getLibrary(t *testing.T, baseURL, token string) []libraryOffer {
	t.Helper()
	resp := doJSON(t, http.MethodGet, baseURL+"/api/library", token, nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET /api/library must return 200")
	var payload struct {
		Library []libraryOffer `json:"library"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	return payload.Library
}

func createOffer(t *testing.T, baseURL, token, name, url string) libraryOffer {
	t.Helper()
	resp := doJSON(t, http.MethodPost, baseURL+"/api/library", token, map[string]any{
		"name":              name,
		"url":               url,
		"icon":              name,
		"description":       name + " desc",
		"suggestedCategory": "Media",
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "POST /api/library must return 201")
	var o libraryOffer
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&o))
	return o
}

// A8 — admin can CRUD the library (POST appends, PATCH edits, PUT /order
// reorders, DELETE is idempotent); a non-admin gets 403 on each write verb.
func TestAdminCanCRUDLibrary_NonAdmin403(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	plex := createOffer(t, s.URL, "admin-session", "Plex", "https://plex.example.com")
	assert.Equal(t, 0, plex.SortIndex, "first offer appends at sortIndex 0")
	sonarr := createOffer(t, s.URL, "admin-session", "Sonarr", "https://sonarr.example.com")
	assert.Equal(t, 1, sonarr.SortIndex, "second offer appends at sortIndex 1")

	// PATCH renames the first offer (200).
	patch := doJSON(t, http.MethodPatch, s.URL+"/api/library/"+plex.ID, "admin-session", map[string]any{"name": "Plex Media Server"})
	require.Equal(t, http.StatusOK, patch.StatusCode)
	var patched libraryOffer
	require.NoError(t, json.NewDecoder(patch.Body).Decode(&patched))
	patch.Body.Close()
	assert.Equal(t, "Plex Media Server", patched.Name)

	// PUT /order reverses the two (204).
	order := doJSON(t, http.MethodPut, s.URL+"/api/library/order", "admin-session", map[string]any{"order": []string{sonarr.ID, plex.ID}})
	require.Equal(t, http.StatusNoContent, order.StatusCode)
	order.Body.Close()
	lib := getLibrary(t, s.URL, "admin-session")
	require.Len(t, lib, 2)
	assert.Equal(t, sonarr.ID, lib[0].ID, "PUT /order rewrote sort_index by position")
	assert.Equal(t, plex.ID, lib[1].ID)

	// DELETE is idempotent (204 both times); the offer is gone after.
	del := doJSON(t, http.MethodDelete, s.URL+"/api/library/"+sonarr.ID, "admin-session", nil)
	require.Equal(t, http.StatusNoContent, del.StatusCode)
	del.Body.Close()
	del2 := doJSON(t, http.MethodDelete, s.URL+"/api/library/"+sonarr.ID, "admin-session", nil)
	require.Equal(t, http.StatusNoContent, del2.StatusCode, "DELETE library offer is idempotent")
	del2.Body.Close()
	require.Len(t, getLibrary(t, s.URL, "admin-session"), 1)

	// A non-admin is forbidden on every write verb (403), never 401.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/library", map[string]any{"name": "x", "url": "https://x"}},
		{http.MethodPatch, "/api/library/" + plex.ID, map[string]any{"name": "y"}},
		{http.MethodPut, "/api/library/order", map[string]any{"order": []string{plex.ID}}},
		{http.MethodDelete, "/api/library/" + plex.ID, nil},
	} {
		resp := doJSON(t, c.method, s.URL+c.path, "non-admin-session", c.body)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "%s %s by non-admin must be 403", c.method, c.path)
		resp.Body.Close()
	}
}

// A9 — any authenticated user can browse the library and gets offers in
// sort_index order with the `added` flag (false until they hold a copy).
func TestAnyUserCanBrowseLibrary_Ordered(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	createOffer(t, s.URL, "admin-session", "Plex", "https://plex.example.com")
	createOffer(t, s.URL, "admin-session", "Sonarr", "https://sonarr.example.com")
	createOffer(t, s.URL, "admin-session", "Radarr", "https://radarr.example.com")

	lib := getLibrary(t, s.URL, "non-admin-session")
	require.Len(t, lib, 3)
	assert.Equal(t, []string{"Plex", "Sonarr", "Radarr"}, []string{lib[0].Name, lib[1].Name, lib[2].Name},
		"offers come back in sort_index order")
	for _, o := range lib {
		assert.False(t, o.Added, "no copies held yet → added=false")
	}
}
