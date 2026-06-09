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

// AC A6 — Admin can CRUD catalog; non-admin gets 403.

func TestUserCannotCreateService_403(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	body, _ := json.Marshal(map[string]any{
		"slug":        "gitea",
		"name":        "Gitea",
		"description": "Git hosting",
		"url":         "https://gitea.kube.calebdunn.tech",
		"icon":        "gitea",
		"gatus_key":   "core_gitea",
	})
	req, _ := http.NewRequest(http.MethodPost, s.URL+"/api/services", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "non-admin-session"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"non-admin POST /api/services must return 403")
}

func TestAdminCanCreateService_201(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	body, _ := json.Marshal(map[string]any{
		"slug":        "gitea",
		"name":        "Gitea",
		"description": "Git hosting",
		"url":         "https://gitea.kube.calebdunn.tech",
		"icon":        "gitea",
		"gatus_key":   "core_gitea",
	})
	req, _ := http.NewRequest(http.MethodPost, s.URL+"/api/services", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusCreated, resp.StatusCode,
		"admin POST /api/services must return 201")
}

func TestAdminCanEditService_200(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	// Target a real seeded entry (catalog uses generated UUIDs).
	svcs := listServices(t, s.URL, "admin-session")
	require.NotEmpty(t, svcs, "expected seeded services in the catalog")
	target := svcs[0].ID

	body, _ := json.Marshal(map[string]any{"name": "Gitea (renamed)"})
	req, _ := http.NewRequest(http.MethodPatch, s.URL+"/api/services/"+target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode,
		"admin PATCH /api/services/{id} must return 200")

	var got struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	assert.Equal(t, target, got.ID)
	assert.Equal(t, "Gitea (renamed)", got.Name, "PATCH must apply the new name")
}

func TestUserCannotEditService_403(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	svcs := listServices(t, s.URL, "non-admin-session")
	require.NotEmpty(t, svcs)

	body, _ := json.Marshal(map[string]any{"name": "nope"})
	req, _ := http.NewRequest(http.MethodPatch, s.URL+"/api/services/"+svcs[0].ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "non-admin-session"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"non-admin PATCH /api/services/{id} must return 403")
}

func TestAdminCanDeleteService_204(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	svcs := listServices(t, s.URL, "admin-session")
	require.NotEmpty(t, svcs, "expected seeded services in the catalog")
	target := svcs[0].ID

	req, _ := http.NewRequest(http.MethodDelete, s.URL+"/api/services/"+target, nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusNoContent, resp.StatusCode,
		"admin DELETE /api/services/{id} must return 204")

	// Deleting again is now a 404 — the entry is gone.
	req2, _ := http.NewRequest(http.MethodDelete, s.URL+"/api/services/"+target, nil)
	req2.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp2.StatusCode,
		"deleting an already-deleted service must return 404")
}

func TestUserCannotDeleteService_403(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	svcs := listServices(t, s.URL, "non-admin-session")
	require.NotEmpty(t, svcs)

	req, _ := http.NewRequest(http.MethodDelete, s.URL+"/api/services/"+svcs[0].ID, nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "non-admin-session"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"non-admin DELETE /api/services/{id} must return 403")
}
