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

func TestMarkFavoritePersistsAcrossSessions(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	// Session 1: mark a favorite.
	req1, _ := http.NewRequest(http.MethodPost, s.URL+"/api/favorites/service-id-1", nil)
	req1.AddCookie(&http.Cookie{Name: "homepad_session", Value: "session-one"})
	resp1, err := http.DefaultClient.Do(req1)
	require.NoError(t, err)
	defer resp1.Body.Close()
	require.Equal(t, http.StatusNoContent, resp1.StatusCode,
		"POST /api/favorites/{id} must return 204")

	// Session 2 (same user, new login): favorite should still be there in /api/services payload.
	req2, _ := http.NewRequest(http.MethodGet, s.URL+"/api/services", nil)
	req2.AddCookie(&http.Cookie{Name: "homepad_session", Value: "session-two"})
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode, "GET /api/services must return 200")

	var payload struct {
		Services []struct {
			ID       string `json:"id"`
			Favorite bool   `json:"favorite"`
		} `json:"services"`
	}
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&payload))

	var found bool
	for _, svc := range payload.Services {
		if svc.ID == "service-id-1" {
			found = svc.Favorite
		}
	}
	assert.True(t, found, "favorite marked in session 1 must persist into session 2")
}

func TestPersonalSortOrderPersistsAcrossSessions(t *testing.T) {
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
