package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC cap6-uptime-display-toggle §7 — the global admin System setting that
// gates the app-grid uptime display. GET /api/system/config is public (D4) and
// defaults to ON when no row exists (D7/AC-008); PATCH /api/admin/settings is
// admin-only (D5) and upserts the singleton row. This exercises the HTTP
// contract end-to-end against a live Postgres, so a missing route fails on the
// status assertion (RED) rather than a compile error.

type sysConfigBody struct {
	ShowUptimeDisplay bool `json:"showUptimeDisplay"`
}

func getSystemConfig(t *testing.T, baseURL string) (*http.Response, sysConfigBody) {
	t.Helper()
	// No cookie — the config endpoint is public (AC-010).
	resp, err := http.Get(baseURL + "/api/system/config")
	require.NoError(t, err)
	var body sysConfigBody
	if resp.StatusCode == http.StatusOK {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	}
	return resp, body
}

// AC-008 — with no system_settings row (fresh install), GET /api/system/config
// returns the default-ON config.
func TestSystemConfig_DefaultsOnWhenNoRow(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	resp, body := getSystemConfig(t, s.URL)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET /api/system/config must return 200")
	assert.True(t, body.ShowUptimeDisplay, "default (no row) must be showUptimeDisplay=true")
}

// AC-010 — GET /api/system/config requires no authentication (accessible
// before login). getSystemConfig sends no cookie; a 200 proves it is public.
func TestSystemConfig_PublicNoAuth(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	resp, _ := getSystemConfig(t, s.URL)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "GET /api/system/config must be reachable without a session")
}

// AC-011 + AC-009 — an admin PATCH of showUptimeDisplay=false returns 200 with
// the updated config, and a subsequent GET reflects the persisted value.
func TestAdminSettings_AdminTogglesOff(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	resp := doJSON(t, http.MethodPatch, s.URL+"/api/admin/settings", "admin-session",
		map[string]any{"showUptimeDisplay": false})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "admin PATCH must return 200")
	var patched sysConfigBody
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&patched))
	assert.False(t, patched.ShowUptimeDisplay, "PATCH response must echo the saved value")

	getResp, body := getSystemConfig(t, s.URL)
	defer getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	assert.False(t, body.ShowUptimeDisplay, "GET after save must return the persisted OFF value (AC-009)")
}

// UTC-5 / AC-006 — toggling back ON persists and reads back true.
func TestAdminSettings_ReEnable(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	off := doJSON(t, http.MethodPatch, s.URL+"/api/admin/settings", "admin-session",
		map[string]any{"showUptimeDisplay": false})
	off.Body.Close()
	require.Equal(t, http.StatusOK, off.StatusCode)

	on := doJSON(t, http.MethodPatch, s.URL+"/api/admin/settings", "admin-session",
		map[string]any{"showUptimeDisplay": true})
	defer on.Body.Close()
	require.Equal(t, http.StatusOK, on.StatusCode)
	var reenabled sysConfigBody
	require.NoError(t, json.NewDecoder(on.Body).Decode(&reenabled))
	assert.True(t, reenabled.ShowUptimeDisplay, "re-enabling must persist showUptimeDisplay=true")

	_, body := getSystemConfig(t, s.URL)
	assert.True(t, body.ShowUptimeDisplay)
}

// AC-012 — a non-admin authenticated PATCH is forbidden.
func TestAdminSettings_NonAdminForbidden(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	resp := doJSON(t, http.MethodPatch, s.URL+"/api/admin/settings", "non-admin-session",
		map[string]any{"showUptimeDisplay": false})
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "non-admin PATCH must return 403")
}

// AC-013 — an unauthenticated PATCH is rejected with 401.
func TestAdminSettings_UnauthenticatedRejected(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	// doJSON always attaches a cookie; an unknown token is an unbound (absent)
	// session, so the server sees no authenticated user.
	resp := doJSON(t, http.MethodPatch, s.URL+"/api/admin/settings", "no-such-session",
		map[string]any{"showUptimeDisplay": false})
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "unauthenticated PATCH must return 401")
}
