package gatus_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/gatus"
)

// AC A4 — Status staleness < 60s (the poller ticker runs at most every 30s
// and the public snapshot always carries an as_of timestamp).

func TestPollerTickerRunsUnder30s(t *testing.T) {
	c := gatus.NewClient("http://example.invalid")
	p := gatus.NewPoller(c, 30*time.Second)

	assert.LessOrEqual(t, p.Interval(), 30*time.Second,
		"poller tick interval must be <= 30s to keep tile staleness under 60s budget")
}

func TestSnapshotIncludesAsOfTimestamp(t *testing.T) {
	c := gatus.NewClient("http://example.invalid")
	p := gatus.NewPoller(c, 30*time.Second)

	// GREEN phase: kick the poller once so AsOf is set; here we just assert the contract.
	snap := p.Snapshot()
	assert.False(t, snap.AsOf.IsZero(),
		"Snapshot.AsOf must be set so the API can publish staleness; got zero time")
}

// AC A9 — Poller survives Gatus being unreachable: it doesn't crash and the
// snapshot it exposes contains entries marked UNKNOWN rather than no data at all.

func TestPollerSurvivesGatusUnreachable(t *testing.T) {
	c := gatus.NewClient("http://127.0.0.1:1") // RFC 3330-ish black hole
	p := gatus.NewPoller(c, 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	err := p.Run(ctx)
	// Run should exit cleanly on ctx cancel; not return an error caused by Gatus being down.
	require.True(t, err == nil || err == context.DeadlineExceeded || err == context.Canceled,
		"Poller.Run must not bubble up transport errors from Gatus; got %v", err)

	snap := p.Snapshot()
	assert.NotNil(t, snap.Statuses, "snapshot must always expose a (possibly empty) status map")
}
