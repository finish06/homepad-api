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

// AC A1 — Register / login / logout with email + password.

func TestRegisterCreatesUser(t *testing.T) {
	s := testsupport.NewServer()
	defer s.Close()

	body, _ := json.Marshal(map[string]string{
		"email":    "alice@example.com",
		"password": "correct horse battery staple",
	})
	resp, err := http.Post(s.URL+"/api/register", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusCreated, resp.StatusCode,
		"POST /api/register must return 201 Created")
}

func TestLoginSetsSessionCookie(t *testing.T) {
	s := testsupport.NewServer()
	defer s.Close()

	// GREEN phase will register first; this skeleton just asserts shape.
	body, _ := json.Marshal(map[string]string{
		"email":    "alice@example.com",
		"password": "correct horse battery staple",
	})
	resp, err := http.Post(s.URL+"/api/login", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode,
		"POST /api/login with valid creds must return 200")

	var sess *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "homepad_session" {
			sess = c
		}
	}
	require.NotNil(t, sess, "login must set a homepad_session cookie")
	assert.True(t, sess.HttpOnly, "session cookie must be HttpOnly")
	assert.Equal(t, http.SameSiteLaxMode, sess.SameSite, "session cookie should be SameSite=Lax for v1 same-domain deploy")
}

func TestMeUnauthorized(t *testing.T) {
	s := testsupport.NewServer()
	defer s.Close()

	resp, err := http.Get(s.URL + "/api/me")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"GET /api/me without session must return 401")
}

func TestMeAuthorized(t *testing.T) {
	s := testsupport.NewServer()
	defer s.Close()

	// GREEN phase: full register → login → use returned cookie.
	req, _ := http.NewRequest(http.MethodGet, s.URL+"/api/me", nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "real-once-impl-exists"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"GET /api/me with a valid session cookie must return 200")
}

func TestLogoutClearsSession(t *testing.T) {
	s := testsupport.NewServer()
	defer s.Close()

	req, _ := http.NewRequest(http.MethodPost, s.URL+"/api/logout", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNoContent, resp.StatusCode,
		"POST /api/logout must return 204 No Content")
}
