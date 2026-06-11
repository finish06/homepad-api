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

// v3 theme-mode backend slice — ACs A2 (default), A5 (per-user persistence),
// A6 (validation), A7 (session-gated, writes only the caller's row).

// meView is the GET /api/me / PATCH /api/me response shape — the existing
// userView plus the new themePref field.
type meView struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	ThemePref string `json:"themePref"`
}

func getMe(t *testing.T, baseURL, session string) (int, meView) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/api/me", nil)
	if session != "" {
		req.AddCookie(&http.Cookie{Name: "homepad_session", Value: session})
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var mv meView
	if resp.StatusCode == http.StatusOK {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&mv))
	}
	return resp.StatusCode, mv
}

func patchMe(t *testing.T, baseURL, session string, body any) (int, meView) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPatch, baseURL+"/api/me", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.AddCookie(&http.Cookie{Name: "homepad_session", Value: session})
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var mv meView
	if resp.StatusCode == http.StatusOK {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&mv))
	}
	return resp.StatusCode, mv
}

// A2 — a brand-new user defaults to themePref "system", surfaced both on the
// register response and on GET /api/me.
func TestThemePrefDefaultsToSystem(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	body, _ := json.Marshal(map[string]string{
		"email":    "themer@example.com",
		"password": "correct horse battery staple",
	})
	resp, err := http.Post(s.URL+"/api/register", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var reg meView
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&reg))
	assert.Equal(t, "system", reg.ThemePref, "register response must default themePref to system")

	// And the same default is visible on GET /api/me for a seeded user.
	code, mv := getMe(t, s.URL, "any-user")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "system", mv.ThemePref, "GET /api/me must report default themePref system")
}

// A5 — the preference persists per-user across sessions: set dark in one
// session, read it back in a fresh session for the same user.
func TestThemePrefPersistsAcrossSessions(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	code, mv := patchMe(t, s.URL, "session-one", map[string]string{"themePref": "dark"})
	require.Equal(t, http.StatusOK, code, "PATCH /api/me with a session must return 200")
	assert.Equal(t, "dark", mv.ThemePref, "PATCH response must echo the updated themePref")

	// session-two is the same user as session-one (testsupport binds both to
	// user@homepad.test) — proves the value lives on the account, not the session.
	code, mv = getMe(t, s.URL, "session-two")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "dark", mv.ThemePref, "themePref must persist per-user across sessions")
}

// A6 — only system|light|dark are accepted; any other value is rejected with
// 400 and leaves the stored value unchanged.
func TestThemePrefRejectsInvalidValue(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	// Establish a known prior value.
	code, _ := patchMe(t, s.URL, "any-user", map[string]string{"themePref": "light"})
	require.Equal(t, http.StatusOK, code)

	for _, bad := range []string{"neon", "Dark", "", "SYSTEM"} {
		code, _ := patchMe(t, s.URL, "any-user", map[string]string{"themePref": bad})
		assert.Equalf(t, http.StatusBadRequest, code, "PATCH themePref=%q must be 400", bad)
	}

	// The stored value is untouched by the rejected writes.
	code, mv := getMe(t, s.URL, "any-user")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "light", mv.ThemePref, "a rejected PATCH must not change the stored themePref")
}

// A7a — PATCH /api/me requires a session; unauthenticated callers get 401 and
// nothing is written.
func TestThemePrefRequiresSession(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	code, _ := patchMe(t, s.URL, "", map[string]string{"themePref": "dark"})
	assert.Equal(t, http.StatusUnauthorized, code, "PATCH /api/me without a session must return 401")
}

// A7b — a user can change only their own theme: writing as one user must not
// touch another user's row.
func TestThemePrefWritesOnlyCurrentUser(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	// non-admin-session is the regular user; admin-session is the admin.
	code, _ := patchMe(t, s.URL, "non-admin-session", map[string]string{"themePref": "dark"})
	require.Equal(t, http.StatusOK, code)

	// The admin's row is unaffected — still the default.
	code, admin := getMe(t, s.URL, "admin-session")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "system", admin.ThemePref, "writing one user's theme must not change another user's row")
}
