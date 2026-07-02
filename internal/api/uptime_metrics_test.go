package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/gatus"
)

// AC-U02 — a service with no gatus_key maps to an empty (non-nil) map so the JSON
// wire form is "uptimeWindows": {} (never null), and the frontend shows no uptime UI.
func TestUptimeWindowsForNoKeyIsEmptyNotNil(t *testing.T) {
	snap := gatus.Snapshot{Statuses: map[string]gatus.EndpointStatus{}}

	got := uptimeWindowsFor(snap, "")
	require.NotNil(t, got, "no gatus_key must yield {} (non-nil) so JSON is {} not null")
	assert.Len(t, got, 0)
}

// AC-U03 — a gatus_key with no snapshot entry (Gatus unreachable / no data) also
// maps to {}.
func TestUptimeWindowsForMissingSnapshotEntryIsEmpty(t *testing.T) {
	snap := gatus.Snapshot{Statuses: map[string]gatus.EndpointStatus{}}

	got := uptimeWindowsFor(snap, "core_gitea")
	require.NotNil(t, got)
	assert.Len(t, got, 0)
}

// AC-U01/U04 — a populated snapshot surfaces the window fractions verbatim.
func TestUptimeWindowsForPopulatedPreservesFractions(t *testing.T) {
	snap := gatus.Snapshot{Statuses: map[string]gatus.EndpointStatus{
		"core_gitea": {Key: "core_gitea", Uptime: map[string]float64{
			"24h": 1.0,
			"7d":  0.945815,
			"30d": 0.9981,
		}},
	}}

	got := uptimeWindowsFor(snap, "core_gitea")
	require.Len(t, got, 3)
	assert.InDelta(t, 1.0, got["24h"], 1e-9)
	assert.InDelta(t, 0.945815, got["7d"], 1e-9)
	assert.InDelta(t, 0.9981, got["30d"], 1e-9)
}

// AC-U03 — a partial snapshot (only some windows answered) surfaces exactly the
// windows present; absent windows stay absent (not 0).
func TestUptimeWindowsForPartialOmitsMissing(t *testing.T) {
	snap := gatus.Snapshot{Statuses: map[string]gatus.EndpointStatus{
		"core_gitea": {Key: "core_gitea", Uptime: map[string]float64{"24h": 1.0}},
	}}

	got := uptimeWindowsFor(snap, "core_gitea")
	require.Len(t, got, 1)
	_, has7d := got["7d"]
	assert.False(t, has7d, "a window Gatus never answered must be absent, not 0")
}
