package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// v7 ux-redesign §6.2 + §11 — the avatar renders real initials derived from a
// display name. /api/me previously returned {id, email, role, themePref} with no
// name field; this AC adds a nullable display name surfaced as `name` so the
// frontend can derive initials (with email fallback when name is empty).

// §6.2 — GET /api/me must include a `name` field. A user with no display name
// set surfaces it as an empty string (the frontend then falls back to the
// email's first letter); the key is always present in the contract.
func TestMeIncludesNameField(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	req, _ := http.NewRequest(http.MethodGet, s.URL+"/api/me", nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "any-user"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	_, present := got["name"]
	assert.True(t, present, "GET /api/me must include a `name` key (got %s)", raw)
	assert.Equal(t, "", got["name"], "a user with no display name must surface name as empty string")
}

// §6.2 — the register response surfaces the same `name` field (empty for a
// brand-new account with no display name), keeping the user view contract
// consistent across endpoints.
func TestRegisterResponseIncludesNameField(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	body, _ := json.Marshal(map[string]string{
		"email":    "named@example.com",
		"password": "correct horse battery staple",
	})
	resp, err := http.Post(s.URL+"/api/register", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	_, present := got["name"]
	assert.True(t, present, "register response must include a `name` key (got %s)", raw)
}
