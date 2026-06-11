package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// AC A5 — Per-user favorites + manual sort order persist across sessions.

// listServices returns the catalog as seen by the session token's user.
func listServices(t *testing.T, baseURL, token string) []struct {
	ID       string `json:"id"`
	Favorite bool   `json:"favorite"`
} {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/api/services", nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: token})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET /api/services must return 200")

	var payload struct {
		Services []struct {
			ID       string `json:"id"`
			Favorite bool   `json:"favorite"`
		} `json:"services"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	return payload.Services
}

func TestMarkFavoritePersistsAcrossSessions(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	// Pick a real seeded service id (catalog uses generated UUIDs).
	svcs := listServices(t, s.URL, "session-one")
	require.NotEmpty(t, svcs, "expected seeded services in the catalog")
	target := svcs[0].ID

	// Session 1: mark it as a favorite.
	req1, _ := http.NewRequest(http.MethodPost, s.URL+"/api/favorites/"+target, nil)
	req1.AddCookie(&http.Cookie{Name: "homepad_session", Value: "session-one"})
	resp1, err := http.DefaultClient.Do(req1)
	require.NoError(t, err)
	defer resp1.Body.Close()
	require.Equal(t, http.StatusNoContent, resp1.StatusCode,
		"POST /api/favorites/{id} must return 204")

	// Session 2 (same user, new login): favorite still present in /api/services.
	var found bool
	for _, svc := range listServices(t, s.URL, "session-two") {
		if svc.ID == target {
			found = svc.Favorite
		}
	}
	assert.True(t, found, "favorite marked in session 1 must persist into session 2")
}

func TestPersonalSortOrderPersistsAcrossSessions(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	// The catalog seeds two services (Gitea, Grafana) with generated UUIDs and
	// defaults to name order. Save the reverse, then prove a fresh session sees it.
	def := listServices(t, s.URL, "session-one")
	require.Len(t, def, 2, "expected the two seeded services in default (name) order")
	reversed := []string{def[1].ID, def[0].ID}

	order, _ := json.Marshal(map[string]any{"order": reversed})
	req1, _ := http.NewRequest(http.MethodPut, s.URL+"/api/layout", bytes.NewReader(order))
	req1.Header.Set("Content-Type", "application/json")
	req1.AddCookie(&http.Cookie{Name: "homepad_session", Value: "session-one"})
	resp1, err := http.DefaultClient.Do(req1)
	require.NoError(t, err)
	defer resp1.Body.Close()
	require.Equal(t, http.StatusNoContent, resp1.StatusCode,
		"PUT /api/layout must return 204")

	// Session 2 (same user, new login): /api/services honors the saved order.
	got := listServices(t, s.URL, "session-two")
	require.Len(t, got, 2, "expected both services back in the user's layout order")
	assert.Equal(t, reversed[0], got[0].ID, "saved order must persist (position 0)")
	assert.Equal(t, reversed[1], got[1].ID, "saved order must persist (position 1)")
}

// AC A5 (cont.) — un-favoriting via DELETE clears the star and persists across
// sessions. Closes the coverage gap on handleRemoveFavorite / storage.RemoveFavorite.
func TestRemoveFavoritePersistsAcrossSessions(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	svcs := listServices(t, s.URL, "session-one")
	require.NotEmpty(t, svcs, "expected seeded services in the catalog")
	target := svcs[0].ID

	// Mark, then remove the favorite (same idempotent verb pattern as the UI).
	mark, _ := http.NewRequest(http.MethodPost, s.URL+"/api/favorites/"+target, nil)
	mark.AddCookie(&http.Cookie{Name: "homepad_session", Value: "session-one"})
	mresp, err := http.DefaultClient.Do(mark)
	require.NoError(t, err)
	mresp.Body.Close()
	require.Equal(t, http.StatusNoContent, mresp.StatusCode)

	del, _ := http.NewRequest(http.MethodDelete, s.URL+"/api/favorites/"+target, nil)
	del.AddCookie(&http.Cookie{Name: "homepad_session", Value: "session-one"})
	dresp, err := http.DefaultClient.Do(del)
	require.NoError(t, err)
	defer dresp.Body.Close()
	require.Equal(t, http.StatusNoContent, dresp.StatusCode,
		"DELETE /api/favorites/{id} must return 204")

	// Fresh session (same user): the favorite is gone.
	for _, svc := range listServices(t, s.URL, "session-two") {
		if svc.ID == target {
			assert.False(t, svc.Favorite, "favorite must be cleared after DELETE")
		}
	}
}
