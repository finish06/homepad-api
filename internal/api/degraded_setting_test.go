package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// The "Slow" threshold (DEGRADED = succeeded but slower than N ms) is a System
// setting: read on GET /api/system/config as statusDegradedMs (the EFFECTIVE
// value — the stored one, else the server default), written by admins via PATCH
// /api/admin/settings, and applied to the poller immediately (no restart).

func getSysCfg(t *testing.T, baseURL string) map[string]any {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/system/config")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func TestSystemConfig_StatusDegradedMsDefaults(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	cfg := getSysCfg(t, s.URL)
	assert.EqualValues(t, 1000, cfg["statusDegradedMs"], "no row → the server default (1s)")
}

func TestAdminSettings_SetStatusDegradedMs(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	resp := doJSON(t, http.MethodPatch, s.URL+"/api/admin/settings", "admin-session", map[string]any{"statusDegradedMs": 250})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.EqualValues(t, 250, out["statusDegradedMs"])
	assert.Equal(t, true, out["showUptimeDisplay"], "an untouched field keeps its value")
	assert.EqualValues(t, 250, getSysCfg(t, s.URL)["statusDegradedMs"], "persisted")
}

func TestAdminSettings_StatusDegradedMsValidation(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	for _, bad := range []any{-1, 600001, "fast"} {
		resp := doJSON(t, http.MethodPatch, s.URL+"/api/admin/settings", "admin-session", map[string]any{"statusDegradedMs": bad})
		resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "statusDegradedMs=%v must be rejected", bad)
	}
	assert.EqualValues(t, 1000, getSysCfg(t, s.URL)["statusDegradedMs"], "rejected values leave the setting alone")
}

func TestAdminSettings_StatusDegradedMsAppliesToPollerImmediately(t *testing.T) {
	// A 41 ms check is UP under the 1000 ms default, DEGRADED once an admin sets
	// the threshold to 10 ms — with no restart, on the next (manual) re-poll.
	s, _ := testsupport.NewServerWithGatus(t, map[string][]testsupport.GatusResult{
		"core_gitea": {{Success: true, DurationMs: 41}},
	})
	defer s.Close()
	refresh := func() {
		req, _ := http.NewRequest(http.MethodPost, s.URL+"/api/status/refresh", nil)
		req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "any-user"})
		r, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		r.Body.Close()
	}
	statusOf := func(slug string) string {
		req, _ := http.NewRequest(http.MethodGet, s.URL+"/api/services", nil)
		req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "any-user"})
		r, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer r.Body.Close()
		var p struct {
			Services []map[string]any `json:"services"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&p))
		for _, sv := range p.Services {
			if sv["slug"] == slug {
				return sv["status"].(string)
			}
		}
		return "?"
	}
	refresh()
	require.Equal(t, "UP", statusOf("gitea"))

	resp := doJSON(t, http.MethodPatch, s.URL+"/api/admin/settings", "admin-session", map[string]any{"statusDegradedMs": 10})
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	refresh()
	assert.Equal(t, "DEGRADED", statusOf("gitea"))

	// 0 disables the derivation entirely.
	resp = doJSON(t, http.MethodPatch, s.URL+"/api/admin/settings", "admin-session", map[string]any{"statusDegradedMs": 0})
	resp.Body.Close()
	refresh()
	assert.Equal(t, "UP", statusOf("gitea"))
}
