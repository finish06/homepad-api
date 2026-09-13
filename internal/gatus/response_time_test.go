package gatus_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/gatus"
)

// SPEC-tile-density OQ-6 — the compact tile's status line ("Online · 41 ms")
// needs the most recent check's response time. Gatus already reports it per
// result as `duration` (nanoseconds); the client just has to keep it.

func gatusStub(t *testing.T, results []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/endpoints/statuses" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"key": "core_gitea", "results": results}})
	}))
}

func TestFetchAllKeepsResultDuration(t *testing.T) {
	srv := gatusStub(t, []map[string]any{
		{"success": true, "timestamp": time.Date(2026, 9, 13, 1, 0, 0, 0, time.UTC), "duration": int64(41_000_000)},
		{"success": true, "timestamp": time.Date(2026, 9, 13, 1, 0, 30, 0, time.UTC), "duration": int64(1_900_000_000)},
	})
	defer srv.Close()

	statuses, err := gatus.NewClient(srv.URL).FetchAll(context.Background())
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	require.Len(t, statuses[0].Results, 2)
	assert.Equal(t, 41*time.Millisecond, statuses[0].Results[0].Duration)
	assert.Equal(t, 1900*time.Millisecond, statuses[0].Results[1].Duration)
}

func TestFetchAllMissingDurationIsZero(t *testing.T) {
	// Older Gatus payloads (and the existing test stubs) omit duration. That must
	// read as "unknown", never an error and never a fabricated number.
	srv := gatusStub(t, []map[string]any{
		{"success": true, "timestamp": time.Date(2026, 9, 13, 1, 0, 0, 0, time.UTC)},
	})
	defer srv.Close()

	statuses, err := gatus.NewClient(srv.URL).FetchAll(context.Background())
	require.NoError(t, err)
	require.Len(t, statuses[0].Results, 1)
	assert.Equal(t, time.Duration(0), statuses[0].Results[0].Duration)
}

// SPEC-v24 §12.3, OQ-5 — "Retry now" prods the backend poller rather than
// refetching the same stale payload. PollNow is the primitive the endpoint
// calls: it re-polls synchronously and reports whether Gatus answered.

func TestPollNowPublishesFreshSnapshot(t *testing.T) {
	srv := gatusStub(t, []map[string]any{
		{"success": true, "timestamp": time.Date(2026, 9, 13, 1, 0, 0, 0, time.UTC), "duration": int64(5_000_000)},
	})
	defer srv.Close()

	p := gatus.NewPoller(gatus.NewClient(srv.URL), time.Hour)
	before := p.Snapshot()
	require.Empty(t, before.Statuses, "no poll has run yet")

	snap, err := p.PollNow(context.Background())
	require.NoError(t, err)
	assert.Equal(t, gatus.StatusUp, snap.Statuses["core_gitea"].Status)
	assert.False(t, snap.AsOf.Before(before.AsOf), "AsOf must move forward on a successful re-poll")
	assert.Equal(t, snap, p.Snapshot(), "PollNow must publish what it returns")
}

func TestPollNowUnreachableReportsErrorAndKeepsLastSnapshot(t *testing.T) {
	// First, a good poll from a live stub.
	srv := gatusStub(t, []map[string]any{
		{"success": true, "timestamp": time.Date(2026, 9, 13, 1, 0, 0, 0, time.UTC)},
	})
	p := gatus.NewPoller(gatus.NewClient(srv.URL), time.Hour)
	good, err := p.PollNow(context.Background())
	require.NoError(t, err)
	require.Equal(t, gatus.StatusUp, good.Statuses["core_gitea"].Status)
	srv.Close() // Gatus goes away.

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = p.PollNow(ctx)
	require.Error(t, err, "PollNow must surface that Gatus could not be reached")

	// The endpoint has to distinguish "could not reach Gatus" from "re-polled,
	// still old" (spec §12.3 unresolved note). Keeping the last good snapshot —
	// and its AsOf — is what makes that distinction observable to the client.
	after := p.Snapshot()
	assert.Equal(t, good.AsOf, after.AsOf, "a failed manual re-poll must not clobber the last good snapshot")
	assert.Equal(t, gatus.StatusUp, after.Statuses["core_gitea"].Status)
}
