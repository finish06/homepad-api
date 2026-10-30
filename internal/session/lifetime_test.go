package session_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
)

// AC-010 — lifetimes are a function of role. Adults (admin, user) get 30 days;
// a kid gets 180 days (the family iPad stays signed in), both sliding.
func TestLifetimeByRole(t *testing.T) {
	const day = 24 * time.Hour
	cases := map[string]time.Duration{
		"admin": 30 * day,
		"user":  30 * day,
		"kid":   180 * day,
		// Role matching is exact: a near-miss is unknown, so it gets the
		// shorter lifetime rather than the kid one.
		"Kid":  30 * day,
		"kid ": 30 * day,
		// An unknown role gets the SHORTER adult lifetime, never the longer one.
		"":        30 * day,
		"unknown": 30 * day,
	}
	for role, want := range cases {
		assert.Equal(t, want, session.Lifetime(role), "Lifetime(%q)", role)
	}
	assert.Equal(t, 30*day, session.AdultLifetime)
	assert.Equal(t, 180*day, session.KidLifetime)
	assert.Equal(t, time.Minute, session.TouchInterval)
}

// countingStore wraps a Store and reports each DeleteExpired call.
type countingStore struct {
	session.Store
	calls chan struct{}
	fail  atomic.Bool
}

func (c *countingStore) DeleteExpired(ctx context.Context) (int64, error) {
	c.calls <- struct{}{}
	if c.fail.Load() {
		return 0, errors.New("db down")
	}
	return c.Store.DeleteExpired(ctx)
}

func TestRunSweeper_SweepsAtStartAndOnEveryTick_StopsOnCancel(t *testing.T) {
	cs := &countingStore{Store: session.NewMemoryStore(), calls: make(chan struct{}, 16)}
	cs.fail.Store(true) // an error is logged and the loop keeps going

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		session.RunSweeper(ctx, cs, 10*time.Millisecond, func(string, ...any) {})
	}()

	for i := 0; i < 3; i++ {
		select {
		case <-cs.calls:
		case <-time.After(2 * time.Second):
			t.Fatalf("sweep %d did not happen", i)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunSweeper did not return after its context was cancelled")
	}
}

// The first sweep happens immediately, not an interval after boot, so a
// long-down instance cleans up straight away.
func TestRunSweeper_FirstSweepIsImmediate(t *testing.T) {
	cs := &countingStore{Store: session.NewMemoryStore(), calls: make(chan struct{}, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go session.RunSweeper(ctx, cs, time.Hour, func(string, ...any) {})

	select {
	case <-cs.calls:
	case <-time.After(2 * time.Second):
		t.Fatal("no sweep at start")
	}
}
