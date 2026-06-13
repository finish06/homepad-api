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

// addedService is the serviceView returned/listed for an add-from-library copy
// (the fields v9.2 asserts on).
type addedService struct {
	ID              string  `json:"id"`
	Slug            string  `json:"slug"`
	Name            string  `json:"name"`
	Description     string  `json:"description"`
	URL             string  `json:"url"`
	Icon            string  `json:"icon"`
	CategoryID      *string `json:"categoryId"`
	SourceLibraryID *string `json:"sourceLibraryId"`
}

func addFromLibrary(t *testing.T, baseURL, token, offerID string, body any) *http.Response {
	t.Helper()
	return doJSON(t, http.MethodPost, baseURL+"/api/library/"+offerID+"/add", token, body)
}

func decodeService(t *testing.T, resp *http.Response) addedService {
	t.Helper()
	defer resp.Body.Close()
	var sv addedService
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sv))
	return sv
}

func getServicesWithSource(t *testing.T, baseURL, token string) []addedService {
	t.Helper()
	resp := doJSON(t, http.MethodGet, baseURL+"/api/services", token, nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var payload struct {
		Services []addedService `json:"services"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	return payload.Services
}

// A10 — POST /api/library/{id}/add copies the offer into a new services row
// owned by the caller (fields copied, source_library_id set, slug derived &
// unique), returns the serviceView, and the app appears in GET /api/services;
// a second GET /api/library shows added=true for that offer only.
func TestAddFromLibrary_CopiesOfferOntoCallerDashboard(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	offer := createOffer(t, s.URL, "admin-session", "Plex", "https://plex.example.com")
	other := createOffer(t, s.URL, "admin-session", "Sonarr", "https://sonarr.example.com")

	resp := addFromLibrary(t, s.URL, "non-admin-session", offer.ID, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "add must return 201")
	sv := decodeService(t, resp)
	assert.Equal(t, "Plex", sv.Name)
	assert.Equal(t, offer.URL, sv.URL)
	assert.Equal(t, offer.Icon, sv.Icon)
	assert.Equal(t, offer.Description, sv.Description)
	assert.Equal(t, "plex", sv.Slug, "slug derived from name")
	require.NotNil(t, sv.SourceLibraryID)
	assert.Equal(t, offer.ID, *sv.SourceLibraryID, "provenance set")

	// The copy is now on the caller's dashboard.
	found := false
	for _, x := range getServicesWithSource(t, s.URL, "non-admin-session") {
		if x.ID == sv.ID {
			found = true
			require.NotNil(t, x.SourceLibraryID)
			assert.Equal(t, offer.ID, *x.SourceLibraryID)
		}
	}
	assert.True(t, found, "added copy appears in GET /api/services")

	// added flag: true for the added offer only.
	for _, o := range getLibrary(t, s.URL, "non-admin-session") {
		if o.ID == offer.ID {
			assert.True(t, o.Added, "added offer → added=true")
		}
		if o.ID == other.ID {
			assert.False(t, o.Added, "un-added offer → added=false")
		}
	}
}

// A11 — add lands Uncategorized by default (D4); a valid own categoryId files
// it there; another user's / nonexistent categoryId → 400.
func TestAddFromLibrary_CategoryRules(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	offer := createOffer(t, s.URL, "admin-session", "Jellyfin", "https://jellyfin.example.com")

	// default → Uncategorized
	sv := decodeService(t, addFromLibrary(t, s.URL, "non-admin-session", offer.ID, nil))
	assert.Nil(t, sv.CategoryID, "no body → Uncategorized")

	// own category → filed there
	mine := createCategory(t, s.URL, "non-admin-session", "MyMedia")
	sv2 := decodeService(t, addFromLibrary(t, s.URL, "non-admin-session", offer.ID, map[string]any{"categoryId": mine.ID}))
	require.NotNil(t, sv2.CategoryID)
	assert.Equal(t, mine.ID, *sv2.CategoryID)

	// another user's category → 400
	foreign := createCategory(t, s.URL, "admin-session", "AdminMedia")
	r3 := addFromLibrary(t, s.URL, "non-admin-session", offer.ID, map[string]any{"categoryId": foreign.ID})
	assert.Equal(t, http.StatusBadRequest, r3.StatusCode, "foreign category → 400")
	r3.Body.Close()

	// nonexistent category → 400
	r4 := addFromLibrary(t, s.URL, "non-admin-session", offer.ID, map[string]any{"categoryId": "00000000-0000-0000-0000-000000000000"})
	assert.Equal(t, http.StatusBadRequest, r4.StatusCode, "nonexistent category → 400")
	r4.Body.Close()
}

// A12 — editing an offer does NOT propagate to existing copies (C1); deleting
// an offer leaves copies intact with source_library_id nulled (C1/OQ5).
func TestLibraryEditDelete_DoesNotTouchCopies(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	offer := createOffer(t, s.URL, "admin-session", "Vaultwarden", "https://vw.example.com")
	copy := decodeService(t, addFromLibrary(t, s.URL, "non-admin-session", offer.ID, nil))

	// admin edits the offer
	patch := doJSON(t, http.MethodPatch, s.URL+"/api/library/"+offer.ID, "admin-session",
		map[string]any{"name": "CHANGED", "url": "https://changed.example.com"})
	require.Equal(t, http.StatusOK, patch.StatusCode)
	patch.Body.Close()

	// the copy is unchanged, still pointing at the offer
	var got addedService
	for _, x := range getServicesWithSource(t, s.URL, "non-admin-session") {
		if x.ID == copy.ID {
			got = x
		}
	}
	require.Equal(t, copy.ID, got.ID, "copy still present after edit")
	assert.Equal(t, "Vaultwarden", got.Name, "edit did not propagate to copy")
	assert.Equal(t, offer.URL, got.URL)
	require.NotNil(t, got.SourceLibraryID)
	assert.Equal(t, offer.ID, *got.SourceLibraryID)

	// admin deletes the offer
	del := doJSON(t, http.MethodDelete, s.URL+"/api/library/"+offer.ID, "admin-session", nil)
	require.Equal(t, http.StatusNoContent, del.StatusCode)
	del.Body.Close()

	// copy still present; provenance nulled
	var after addedService
	present := false
	for _, x := range getServicesWithSource(t, s.URL, "non-admin-session") {
		if x.ID == copy.ID {
			after = x
			present = true
		}
	}
	assert.True(t, present, "copy survives offer deletion")
	assert.Nil(t, after.SourceLibraryID, "deleting offer nulls source_library_id (FK)")
}

// A13 — adding the same offer twice yields two independent copies (no
// server-side dedupe, D6); both are deletable.
func TestAddFromLibrary_TwiceYieldsTwoCopies(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	offer := createOffer(t, s.URL, "admin-session", "Radarr", "https://radarr.example.com")

	c1 := decodeService(t, addFromLibrary(t, s.URL, "non-admin-session", offer.ID, nil))
	c2 := decodeService(t, addFromLibrary(t, s.URL, "non-admin-session", offer.ID, nil))
	assert.NotEqual(t, c1.ID, c2.ID, "two distinct copies")
	assert.NotEqual(t, c1.Slug, c2.Slug, "second copy gets a unique slug")
	require.NotNil(t, c1.SourceLibraryID)
	require.NotNil(t, c2.SourceLibraryID)
	assert.Equal(t, offer.ID, *c1.SourceLibraryID)
	assert.Equal(t, offer.ID, *c2.SourceLibraryID)

	count := 0
	for _, x := range getServicesWithSource(t, s.URL, "non-admin-session") {
		if x.SourceLibraryID != nil && *x.SourceLibraryID == offer.ID {
			count++
		}
	}
	assert.Equal(t, 2, count, "both copies present — no dedupe")

	for _, id := range []string{c1.ID, c2.ID} {
		del := doJSON(t, http.MethodDelete, s.URL+"/api/services/"+id, "non-admin-session", nil)
		assert.Equal(t, http.StatusNoContent, del.StatusCode, "each copy is independently deletable")
		del.Body.Close()
	}
}
