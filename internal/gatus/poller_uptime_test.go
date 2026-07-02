package gatus_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/gatus"
)

// AC-U04 — the surfaced windows are exactly Gatus's own supported durations minus
// 1h: 24h, 7d, 30d. Pinning the set guards against silently adding/removing one.
func TestUptimeWindowsAre24h7d30d(t *testing.T) {
	assert.Equal(t, []string{"24h", "7d", "30d"}, gatus.UptimeWindows)
}

// AC-U04 — FetchUptime reads Gatus's computed uptime for one endpoint/window from
// GET /api/v1/endpoints/{key}/uptimes/{window}, which returns a bare fraction float
// as text/plain (e.g. "0.945815"). homepad does NOT recompute this from raw history.
func TestFetchUptimeParsesBareFraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/endpoints/core_gitea/uptimes/7d", r.URL.Path)
		fmt.Fprint(w, "0.945815")
	}))
	t.Cleanup(srv.Close)

	got, err := gatus.NewClient(srv.URL).FetchUptime(context.Background(), "core_gitea", "7d")
	require.NoError(t, err)
	assert.InDelta(t, 0.945815, got, 1e-9)
}

// AC-U05 — an unknown key (Gatus 404 "endpoint not found") is an error the caller can
// skip, not a parsed value. Best-effort: it must not surface as uptime 0.
func TestFetchUptimeErrorsOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "endpoint not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	_, err := gatus.NewClient(srv.URL).FetchUptime(context.Background(), "nope", "24h")
	require.Error(t, err, "a 404 from Gatus must be an error so the poller omits the window, not record 0")
}

// AC-U05 — an unsupported window (Gatus 400 with the "Durations supported:" text body)
// is an error, never a mis-parsed float.
func TestFetchUptimeErrorsOnBadDuration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Durations supported: 30d, 7d, 24h, 1h", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	_, err := gatus.NewClient(srv.URL).FetchUptime(context.Background(), "core_gitea", "5d")
	require.Error(t, err)
}

// fakeGatusWithUptime serves both the statuses list (one endpoint with one result)
// and per-window uptime floats, so a full poll can be exercised end to end.
func fakeGatusWithUptime(t *testing.T, key string, uptimes map[string]float64) *httptest.Server {
	t.Helper()
	statuses := []map[string]any{{
		"key": key,
		"results": []map[string]any{
			{"success": true, "timestamp": time.Date(2026, 7, 2, 18, 0, 0, 0, time.UTC)},
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/endpoints/statuses" {
			_ = json.NewEncoder(w).Encode(statuses)
			return
		}
		prefix := "/api/v1/endpoints/" + key + "/uptimes/"
		if strings.HasPrefix(r.URL.Path, prefix) {
			win := strings.TrimPrefix(r.URL.Path, prefix)
			v, ok := uptimes[win]
			if !ok {
				http.Error(w, "endpoint not found", http.StatusNotFound)
				return
			}
			fmt.Fprintf(w, "%f", v)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// AC-U04/U05 — after a poll, the snapshot's EndpointStatus.Uptime carries the
// long-window fractions Gatus reported (24h/7d/30d), attached to the same key whose
// status/results the statuses call produced. A window Gatus can't answer is omitted.
func TestPollPopulatesUptimeWindows(t *testing.T) {
	srv := fakeGatusWithUptime(t, "core_gitea", map[string]float64{
		"24h": 1.0,
		"7d":  0.945815,
		// 30d intentionally absent -> Gatus 404 -> omitted, not 0.
	})
	p := gatus.NewPoller(gatus.NewClient(srv.URL), time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	require.Eventually(t, func() bool {
		st, ok := p.Snapshot().Statuses["core_gitea"]
		return ok && len(st.Uptime) > 0
	}, time.Second, 10*time.Millisecond, "poll must populate EndpointStatus.Uptime")

	st := p.Snapshot().Statuses["core_gitea"]
	assert.InDelta(t, 1.0, st.Uptime["24h"], 1e-9)
	assert.InDelta(t, 0.945815, st.Uptime["7d"], 1e-9)
	_, has30 := st.Uptime["30d"]
	assert.False(t, has30, "a window Gatus 404s must be omitted from Uptime, not recorded as 0")
}
