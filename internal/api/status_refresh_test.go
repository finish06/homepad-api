package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-v24 §12.3 (OQ-5) — POST /api/status/refresh prods the Gatus poller so a
// STALE health panel can ask for fresh evidence instead of refetching the same
// old payload. Session-gated like GET /api/status.

func TestStatusRefresh_RequiresSession(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	resp, err := http.Post(s.URL+"/api/status/refresh", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestStatusRefresh_GatusUnreachableIs503WithAsOf(t *testing.T) {
	// testsupport points Gatus at a black hole, so the re-poll cannot succeed.
	// The panel needs to tell "could not reach Gatus" apart from "re-polled,
	// still old": that is a 503 carrying the as_of the snapshot still stands at.
	s := testsupport.NewServer(t)
	defer s.Close()

	req, _ := http.NewRequest(http.MethodPost, s.URL+"/api/status/refresh", nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "any-user"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	var body struct {
		Error string `json:"error"`
		AsOf  string `json:"as_of"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "status source unreachable", body.Error)
	assert.NotEmpty(t, body.AsOf)
}

func TestStatusRefresh_LiveGatusIs200WithAsOf(t *testing.T) {
	s, gatusURL := testsupport.NewServerWithGatus(t, map[string][]testsupport.GatusResult{
		"core_gitea": {{Success: true, DurationMs: 41}},
	})
	defer s.Close()
	_ = gatusURL

	req, _ := http.NewRequest(http.MethodPost, s.URL+"/api/status/refresh", nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "any-user"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		AsOf string `json:"as_of"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.NotEmpty(t, body.AsOf)
}

// SPEC-tile-density OQ-6 — responseTimeMs on GET /api/services. Present only
// when the latest check carried a duration; otherwise the key is absent so the
// frontend's "state word alone" degradation holds (no fabricated 0 ms).

func TestServices_ResponseTimeMsFromLatestCheck(t *testing.T) {
	s, _ := testsupport.NewServerWithGatus(t, map[string][]testsupport.GatusResult{
		"core_gitea":   {{Success: true, DurationMs: 900}, {Success: true, DurationMs: 41}},
		"core_grafana": {{Success: false, DurationMs: 0}}, // no duration → no field
	})
	defer s.Close()

	// A manual re-poll so the snapshot is populated deterministically.
	req, _ := http.NewRequest(http.MethodPost, s.URL+"/api/status/refresh", nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "any-user"})
	rr, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	rr.Body.Close()
	require.Equal(t, http.StatusOK, rr.StatusCode)

	req, _ = http.NewRequest(http.MethodGet, s.URL+"/api/services", nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "any-user"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var payload struct {
		Services []map[string]any `json:"services"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	bySlug := map[string]map[string]any{}
	for _, sv := range payload.Services {
		bySlug[sv["slug"].(string)] = sv
	}
	require.Contains(t, bySlug, "gitea")
	require.Contains(t, bySlug, "grafana")

	assert.EqualValues(t, 41, bySlug["gitea"]["responseTimeMs"], "latest check's duration, in ms")
	_, present := bySlug["grafana"]["responseTimeMs"]
	assert.False(t, present, "no duration on the latest check → key absent, never 0")
}
