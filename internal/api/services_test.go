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

// AC A9 — If Gatus is unreachable, the app still loads, all tiles show UNKNOWN,
// and /api/services never returns 5xx.

func TestServicesEndpoint_GatusBlackhole_NoFiveXX(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	req, _ := http.NewRequest(http.MethodGet, s.URL+"/api/services", nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "any-user"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Less(t, resp.StatusCode, 500,
		"/api/services must never return 5xx even when Gatus is unreachable; got %d", resp.StatusCode)
}

func TestServicesEndpoint_GatusBlackhole_AllUnknown(t *testing.T) {
	// GREEN phase will wire a blackhole Gatus URL and seed catalog entries
	// with gatus_key set. Test then asserts every entry's status field is "UNKNOWN".
	s := testsupport.NewServer(t)
	defer s.Close()

	req, _ := http.NewRequest(http.MethodGet, s.URL+"/api/services", nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "any-user"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var payload struct {
		Services []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"services"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	require.NotEmpty(t, payload.Services, "expected seeded services in the catalog")

	for _, svc := range payload.Services {
		assert.Equal(t, "UNKNOWN", svc.Status,
			"with Gatus unreachable, all services must report UNKNOWN (service id=%s)", svc.ID)
	}
}

// AC-003 — a service with an EMPTY gatus_key resolves to "NOT_MONITORED" (a
// configuration gap), while a service whose gatus_key IS set but yields no
// result from Gatus (here: Gatus blackholed) stays "UNKNOWN" (a monitoring
// failure). The two strings are never interchangeable.
func TestServicesEndpoint_NotMonitored_VsUnknown(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	// Create a service with NO gatus_key on the caller's own dashboard. The POST
	// response already carries the resolved status.
	body, _ := json.Marshal(map[string]any{
		"slug": "proxmox", "name": "Proxmox",
		"url": "https://proxmox.example.com", "icon": "proxmox",
		// gatus_key intentionally omitted (empty) → not wired to monitoring
	})
	req, _ := http.NewRequest(http.MethodPost, s.URL+"/api/services", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "any-user"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var created struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	assert.Equal(t, "NOT_MONITORED", created.Status,
		"a service with an empty gatus_key must resolve to NOT_MONITORED (AC-003)")

	// Now list: the unwired service is NOT_MONITORED; the seeded "gitea" (gatus_key
	// set, Gatus unreachable) stays UNKNOWN.
	lreq, _ := http.NewRequest(http.MethodGet, s.URL+"/api/services", nil)
	lreq.AddCookie(&http.Cookie{Name: "homepad_session", Value: "any-user"})
	lresp, err := http.DefaultClient.Do(lreq)
	require.NoError(t, err)
	defer lresp.Body.Close()
	require.Equal(t, http.StatusOK, lresp.StatusCode)

	var payload struct {
		Services []struct {
			Slug   string `json:"slug"`
			Status string `json:"status"`
		} `json:"services"`
	}
	require.NoError(t, json.NewDecoder(lresp.Body).Decode(&payload))

	bySlug := map[string]string{}
	for _, svc := range payload.Services {
		bySlug[svc.Slug] = svc.Status
	}
	assert.Equal(t, "NOT_MONITORED", bySlug["proxmox"],
		"unwired service (no gatus_key) must be NOT_MONITORED (AC-003)")
	assert.Equal(t, "UNKNOWN", bySlug["gitea"],
		"wired service with no Gatus result must stay UNKNOWN, not NOT_MONITORED (AC-002/003)")
}

// AC-009 — /api/services is additive: every service object carries an
// uptimeChecks array. With Gatus unreachable (no snapshot data), it is present
// and empty (never null, never absent) so clients render no sparkline.
func TestServicesEndpoint_UptimeChecksPresentAndEmpty(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	req, _ := http.NewRequest(http.MethodGet, s.URL+"/api/services", nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "any-user"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Decode into a map so a missing "uptimeChecks" key is distinguishable from
	// an empty array.
	var payload struct {
		Services []map[string]json.RawMessage `json:"services"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	require.NotEmpty(t, payload.Services, "expected seeded services in the catalog")

	for _, svc := range payload.Services {
		raw, ok := svc["uptimeChecks"]
		require.True(t, ok, "each service must carry an uptimeChecks field (AC-009)")
		assert.JSONEq(t, "[]", string(raw),
			"with no Gatus data uptimeChecks must be [] (not null/absent); got %s", raw)
	}
}
