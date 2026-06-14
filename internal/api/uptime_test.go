package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/gatus"
)

// AC-009 — a service with no gatus_key maps to an empty (non-nil) slice so the
// JSON wire form is "uptimeChecks": [] (never null), and the frontend renders no
// sparkline.
func TestUptimeChecksForNoKeyIsEmptyNotNil(t *testing.T) {
	snap := gatus.Snapshot{Statuses: map[string]gatus.EndpointStatus{}}

	got := uptimeChecksFor(snap, "")
	require.NotNil(t, got, "no gatus_key must yield [] (non-nil) so JSON is [] not null")
	assert.Len(t, got, 0)
}

// AC-006/009 — a gatus_key with no snapshot entry (Gatus unreachable / no data)
// also maps to [].
func TestUptimeChecksForMissingSnapshotEntryIsEmpty(t *testing.T) {
	snap := gatus.Snapshot{Statuses: map[string]gatus.EndpointStatus{}}

	got := uptimeChecksFor(snap, "core_gitea")
	require.NotNil(t, got)
	assert.Len(t, got, 0)
}

// AC-002/003/010 — a populated snapshot maps each CheckResult to a view that
// preserves success and timestamp, oldest-first.
func TestUptimeChecksForPopulatedPreservesOrderSuccessAndTimestamp(t *testing.T) {
	base := time.Date(2026, 6, 14, 8, 0, 0, 0, time.UTC)
	snap := gatus.Snapshot{Statuses: map[string]gatus.EndpointStatus{
		"core_gitea": {Key: "core_gitea", Results: []gatus.CheckResult{
			{Success: true, Timestamp: base},
			{Success: false, Timestamp: base.Add(time.Minute)},
			{Success: true, Timestamp: base.Add(2 * time.Minute)},
		}},
	}}

	got := uptimeChecksFor(snap, "core_gitea")
	require.Len(t, got, 3)

	wantSuccess := []bool{true, false, true}
	for i, c := range got {
		assert.Equal(t, wantSuccess[i], c.Success, "check %d success", i)
		assert.True(t, c.Timestamp.Equal(base.Add(time.Duration(i)*time.Minute)),
			"check %d timestamp preserved", i)
	}
}
