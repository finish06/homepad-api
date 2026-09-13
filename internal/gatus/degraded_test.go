package gatus_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/gatus"
)

// DEGRADED derivation (Caleb, 2026-09-13). Gatus has no "degraded" state of its
// own — a failed [RESPONSE_TIME] condition just fails the check. homepad derives
// it: the latest check SUCCEEDED but took longer than the threshold. Backs the
// compact tile's "Slow · 1.9 s" line and the health panel's amber LED.

func fetchWith(t *testing.T, threshold time.Duration, success bool, durationNs int64) string {
	t.Helper()
	res := map[string]any{"success": success, "timestamp": time.Date(2026, 9, 13, 1, 0, 0, 0, time.UTC)}
	if durationNs > 0 {
		res["duration"] = durationNs
	}
	srv := gatusStub(t, []map[string]any{res})
	defer srv.Close()
	c := gatus.NewClient(srv.URL)
	c.DegradedAfter = threshold
	statuses, err := c.FetchAll(context.Background())
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	return statuses[0].Status
}

func TestDegradedWhenSucceededButSlow(t *testing.T) {
	assert.Equal(t, gatus.StatusDegraded, fetchWith(t, time.Second, true, int64(1900*time.Millisecond)))
}

func TestUpWhenSucceededWithinThreshold(t *testing.T) {
	assert.Equal(t, gatus.StatusUp, fetchWith(t, time.Second, true, int64(41*time.Millisecond)))
	// Exactly at the threshold is still UP — "slower than", not "at least".
	assert.Equal(t, gatus.StatusUp, fetchWith(t, time.Second, true, int64(time.Second)))
}

func TestDownStaysDownRegardlessOfDuration(t *testing.T) {
	assert.Equal(t, gatus.StatusDown, fetchWith(t, time.Second, false, int64(5*time.Second)))
}

func TestUpWhenDurationUnknown(t *testing.T) {
	// No duration in the payload (older Gatus) → nothing to judge slowness by.
	assert.Equal(t, gatus.StatusUp, fetchWith(t, time.Second, true, 0))
}

func TestZeroThresholdDisablesDegraded(t *testing.T) {
	assert.Equal(t, gatus.StatusUp, fetchWith(t, 0, true, int64(30*time.Second)))
}

func TestDefaultThresholdIsOneSecond(t *testing.T) {
	assert.Equal(t, time.Second, gatus.DefaultDegradedAfter)
}
