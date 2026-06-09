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

	body, _ := json.Marshal(map[string]any{"name": "Gitea (renamed)"})
	req, _ := http.NewRequest(http.MethodPatch, s.URL+"/api/services/some-id", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"admin PATCH /api/services/{id} must return 200")
}

func TestAdminCanDeleteService_204(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	req, _ := http.NewRequest(http.MethodDelete, s.URL+"/api/services/some-id", nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNoContent, resp.StatusCode,
		"admin DELETE /api/services/{id} must return 204")
}
