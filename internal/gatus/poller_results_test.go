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

// fakeGatus serves a single endpoint whose results[] are the given (success,
// minute-offset) pairs, oldest first — mirroring Gatus's own ordering where the
// last entry is the most recent check.
func fakeGatus(t *testing.T, key string, results []struct {
	success bool
	minute  int
}) *httptest.Server {
	t.Helper()
	base := time.Date(2026, 6, 14, 8, 0, 0, 0, time.UTC)
	type res struct {
		Success   bool      `json:"success"`
		Timestamp time.Time `json:"timestamp"`
	}
	payload := []map[string]any{{"key": key, "results": func() []res {
		out := make([]res, 0, len(results))
		for _, r := range results {
			out = append(out, res{Success: r.success, Timestamp: base.Add(time.Duration(r.minute) * time.Minute)})
		}
		return out
	}()}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/endpoints/statuses", r.URL.Path)
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// AC-009/010/002 — FetchAll surfaces the full results[] history (not just the
// last entry) as EndpointStatus.Results, oldest-first, preserving each check's
// success flag and timestamp.
func TestFetchAllSurfacesResultsOldestFirst(t *testing.T) {
	srv := fakeGatus(t, "core_gitea", []struct {
		success bool
		minute  int
	}{
		{true, 0}, {false, 1}, {true, 2}, {true, 3},
	})

	statuses, err := gatus.NewClient(srv.URL).FetchAll(context.Background())
	require.NoError(t, err)
	require.Len(t, statuses, 1)

	got := statuses[0].Results
	require.Len(t, got, 4, "all four historical results must be surfaced")

	wantSuccess := []bool{true, false, true, true}
	base := time.Date(2026, 6, 14, 8, 0, 0, 0, time.UTC)
	for i, r := range got {
		assert.Equal(t, wantSuccess[i], r.Success, "result %d success", i)
		assert.True(t, r.Timestamp.Equal(base.Add(time.Duration(i)*time.Minute)),
			"result %d timestamp must be preserved oldest-first; got %s", i, r.Timestamp)
	}
}

// AC: Cap at 20 — when Gatus returns more than 20, keep the most recent 20,
// still oldest-first within that window.
func TestFetchAllCapsResultsAtTwenty(t *testing.T) {
	pairs := make([]struct {
		success bool
		minute  int
	}, 0, 22)
	for i := 0; i < 22; i++ {
		pairs = append(pairs, struct {
			success bool
			minute  int
		}{success: true, minute: i})
	}
	srv := fakeGatus(t, "core_gitea", pairs)

	statuses, err := gatus.NewClient(srv.URL).FetchAll(context.Background())
	require.NoError(t, err)
	require.Len(t, statuses, 1)

	got := statuses[0].Results
	require.Len(t, got, 20, "results must be capped at 20")

	base := time.Date(2026, 6, 14, 8, 0, 0, 0, time.UTC)
	// The two oldest (minute 0,1) are dropped; the window starts at minute 2.
	assert.True(t, got[0].Timestamp.Equal(base.Add(2*time.Minute)),
		"capped window must keep the most recent 20 (drop the 2 oldest); first kept = minute 2, got %s", got[0].Timestamp)
	assert.True(t, got[19].Timestamp.Equal(base.Add(21*time.Minute)),
		"last kept result must be the newest (minute 21); got %s", got[19].Timestamp)
}

// AC-006 — an endpoint Gatus knows about but has no results for yields an empty
// Results slice (no sparkline downstream), not a panic.
func TestFetchAllNoResultsYieldsEmpty(t *testing.T) {
	srv := fakeGatus(t, "core_gitea", nil)

	statuses, err := gatus.NewClient(srv.URL).FetchAll(context.Background())
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Empty(t, statuses[0].Results, "no Gatus results -> empty Results slice")
}
