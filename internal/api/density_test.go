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

// homepad SPEC-tile-density §5 / decision record OQ-9 — the tile density is a
// per-USER preference held server-side (the decision), not per device. Same
// contract shape as themePref: surfaced on GET /api/me, written via PATCH
// /api/me, enum-checked, session-gated, own row only.

func patchMeJSON(t *testing.T, baseURL, session string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPatch, baseURL+"/api/me", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.AddCookie(&http.Cookie{Name: "homepad_session", Value: session})
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func getMeRaw(t *testing.T, baseURL, session string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/api/me", nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: session})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestDensityPrefDefaultsToCompact(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	code, me := getMeRaw(t, s.URL, "any-user")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "compact", me["densityPref"], "a user who never chose gets the v16 default")
}

func TestDensityPrefPersistsAcrossSessions(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	code, out := patchMeJSON(t, s.URL, "session-one", map[string]any{"densityPref": "list"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "list", out["densityPref"], "PATCH echoes the stored value")
	assert.Equal(t, "system", out["themePref"], "an untouched field keeps its value")

	_, me := getMeRaw(t, s.URL, "session-two")
	assert.Equal(t, "list", me["densityPref"], "the same user on another session sees the choice")
}

func TestDensityPrefRejectsInvalidValue(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	code, _ := patchMeJSON(t, s.URL, "any-user", map[string]any{"densityPref": "huge"})
	assert.Equal(t, http.StatusBadRequest, code)
	_, me := getMeRaw(t, s.URL, "any-user")
	assert.Equal(t, "compact", me["densityPref"], "a rejected value must leave the stored one alone")
}

func TestPatchMeWithNoKnownFieldIs400(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	code, _ := patchMeJSON(t, s.URL, "any-user", map[string]any{"bogus": 1})
	assert.Equal(t, http.StatusBadRequest, code)
}

func TestPatchMeThemeOnlyStillWorks(t *testing.T) {
	// Regression guard for the existing v3 contract now that PATCH /api/me
	// accepts more than one field: themePref alone must keep working and must
	// not disturb densityPref.
	s := testsupport.NewServer(t)
	defer s.Close()
	_, _ = patchMeJSON(t, s.URL, "any-user", map[string]any{"densityPref": "large"})
	code, out := patchMeJSON(t, s.URL, "any-user", map[string]any{"themePref": "dark"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "dark", out["themePref"])
	assert.Equal(t, "large", out["densityPref"])
}

func TestDensityPrefRequiresSession(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	code, _ := patchMeJSON(t, s.URL, "", map[string]any{"densityPref": "list"})
	assert.Equal(t, http.StatusUnauthorized, code)
}

func TestDensityPrefWritesOnlyCurrentUser(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	code, _ := patchMeJSON(t, s.URL, "non-admin-session", map[string]any{"densityPref": "list"})
	require.Equal(t, http.StatusOK, code)
	_, admin := getMeRaw(t, s.URL, "admin-session")
	assert.Equal(t, "compact", admin["densityPref"], "another user's row must be untouched")
}
