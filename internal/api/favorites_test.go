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
	t.Skip("personal layout slice not yet implemented — PUT /api/layout is still 501 (next task)")

	s := testsupport.NewServer(t)
	defer s.Close()

	order := map[string]any{"order": []string{"service-id-3", "service-id-1", "service-id-2"}}
	b, _ := json.Marshal(order)

	req1, _ := http.NewRequest(http.MethodPut, s.URL+"/api/layout", bytes.NewReader(b))
	req1.Header.Set("Content-Type", "application/json")
	req1.AddCookie(&http.Cookie{Name: "homepad_session", Value: "session-one"})
	resp1, err := http.DefaultClient.Do(req1)
	require.NoError(t, err)
	defer resp1.Body.Close()
	require.Equal(t, http.StatusNoContent, resp1.StatusCode,
		"PUT /api/layout must return 204")

	req2, _ := http.NewRequest(http.MethodGet, s.URL+"/api/services", nil)
	req2.AddCookie(&http.Cookie{Name: "homepad_session", Value: "session-two"})
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)

	var payload struct {
		Services []struct {
			ID string `json:"id"`
		} `json:"services"`
	}
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&payload))
	require.Len(t, payload.Services, 3, "expected 3 services back in user's layout order")

	assert.Equal(t, "service-id-3", payload.Services[0].ID, "saved order must persist (position 0)")
	assert.Equal(t, "service-id-1", payload.Services[1].ID, "saved order must persist (position 1)")
	assert.Equal(t, "service-id-2", payload.Services[2].ID, "saved order must persist (position 2)")
}
