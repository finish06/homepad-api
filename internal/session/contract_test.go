package session_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
)

// The session.Store contract (SPEC-kid-accounts AC-010, AC-011; plan TASK-101).
// Every case runs against BOTH implementations — the in-memory store used by
// tests and the Postgres store used in production — so the two cannot drift.

// fakeClock is a concurrency-safe settable clock injected into a store.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// harness is one store under test plus two users it can attach sessions to.
type harness struct {
	store session.Store
	clock *fakeClock
	userA string
	userB string
}

type harnessFactory func(t *testing.T) harness

func storeFactories() map[string]harnessFactory {
	return map[string]harnessFactory{
		"memory":   newMemoryHarness,
		"postgres": newPgHarness,
	}
}

func newMemoryHarness(t *testing.T) harness {
	t.Helper()
	clk := newFakeClock()
	st := session.NewMemoryStore()
	st.Clock = clk.Now
	return harness{
		store: st,
		clock: clk,
		// The memory store does not know about users; any distinct ids do.
		userA: "00000000-0000-0000-0000-00000000000a",
		userB: "00000000-0000-0000-0000-00000000000b",
	}
}

// forEachStore runs fn as a subtest against every implementation.
func forEachStore(t *testing.T, fn func(t *testing.T, h harness)) {
	t.Helper()
	for name, mk := range storeFactories() {
		t.Run(name, func(t *testing.T) {
			fn(t, mk(t))
		})
	}
}

const lifetime = 30 * 24 * time.Hour

func TestStore_CreateThenLookup(t *testing.T) {
	forEachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		start := h.clock.Now()

		token, created, err := h.store.Create(ctx, h.userA, lifetime, "Mozilla/5.0 (iPad)")
		require.NoError(t, err)
		require.NotEmpty(t, token)

		raw, err := base64.RawURLEncoding.DecodeString(token)
		require.NoError(t, err, "the token is URL-safe base64, so it is cookie-safe")
		assert.GreaterOrEqual(t, len(raw), 32, "the token carries at least 256 bits of entropy")

		got, err := h.store.Lookup(ctx, token)
		require.NoError(t, err)
		assert.Equal(t, h.userA, got.UserID)
		assert.Equal(t, "Mozilla/5.0 (iPad)", got.UserAgent)
		assert.True(t, got.CreatedAt.Equal(start), "created_at = now")
		assert.True(t, got.LastSeenAt.Equal(start), "last_seen_at = now")
		assert.True(t, got.ExpiresAt.Equal(start.Add(lifetime)), "expires_at = now + lifetime")
		assert.False(t, got.TouchDue, "a brand-new session has nothing to slide")
		assert.Equal(t, created.Handle, got.Handle)
		assert.True(t, created.ExpiresAt.Equal(got.ExpiresAt))
	})
}

func TestStore_TokensAndHandlesAreUniqueAndUnrelated(t *testing.T) {
	forEachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		t1, s1, err := h.store.Create(ctx, h.userA, lifetime, "")
		require.NoError(t, err)
		t2, s2, err := h.store.Create(ctx, h.userA, lifetime, "")
		require.NoError(t, err)

		assert.NotEqual(t, t1, t2, "each Create mints a fresh token")
		assert.NotEmpty(t, s1.Handle)
		assert.NotEqual(t, s1.Handle, s2.Handle, "each session has its own handle")

		// The handle is an opaque random id: it must not BE the token, contain
		// it, or be the token's hash — anyone who can see a handle (an admin's
		// device list, Phase 3) must learn nothing that authenticates.
		for _, c := range []struct {
			token, handle string
		}{{t1, s1.Handle}, {t2, s2.Handle}} {
			sum := sha256.Sum256([]byte(c.token))
			assert.NotContains(t, c.handle, c.token)
			assert.NotContains(t, c.token, c.handle)
			assert.NotEqual(t, hex.EncodeToString(sum[:]), c.handle)
			assert.NotEqual(t, base64.RawURLEncoding.EncodeToString(sum[:]), c.handle)
		}
	})
}

func TestStore_UnknownTokenIsNotFound(t *testing.T) {
	forEachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		for _, tok := range []string{"", "nope", strings.Repeat("x", 43)} {
			_, err := h.store.Lookup(ctx, tok)
			assert.True(t, errors.Is(err, session.ErrNotFound), "Lookup(%q) = %v, want ErrNotFound", tok, err)
		}
	})
}

func TestStore_ExpiredSessionIsNotFound(t *testing.T) {
	forEachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		token, _, err := h.store.Create(ctx, h.userA, lifetime, "")
		require.NoError(t, err)

		h.clock.Advance(lifetime - time.Second)
		_, err = h.store.Lookup(ctx, token)
		require.NoError(t, err, "one second before expiry the session is still valid")

		h.clock.Advance(time.Second)
		_, err = h.store.Lookup(ctx, token)
		assert.True(t, errors.Is(err, session.ErrNotFound), "at expires_at the session is gone, got %v", err)

		// An expired session cannot be revived by sliding it.
		slid, err := h.store.Touch(ctx, token, lifetime)
		require.NoError(t, err)
		assert.False(t, slid, "Touch must not resurrect an expired session")
		_, err = h.store.Lookup(ctx, token)
		assert.True(t, errors.Is(err, session.ErrNotFound))
	})
}

// The 1-minute write throttle: last_seen_at / expires_at are written at most
// once per TouchInterval per session, however often the client polls.
func TestStore_TouchIsThrottledToOncePerInterval(t *testing.T) {
	forEachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		start := h.clock.Now()
		token, _, err := h.store.Create(ctx, h.userA, lifetime, "")
		require.NoError(t, err)

		h.clock.Advance(session.TouchInterval - time.Second)
		got, err := h.store.Lookup(ctx, token)
		require.NoError(t, err)
		assert.False(t, got.TouchDue, "inside the interval no slide is due")
		slid, err := h.store.Touch(ctx, token, lifetime)
		require.NoError(t, err)
		assert.False(t, slid, "Touch inside the interval must not write")
		got, err = h.store.Lookup(ctx, token)
		require.NoError(t, err)
		assert.True(t, got.LastSeenAt.Equal(start), "last_seen_at unchanged inside the interval")
		assert.True(t, got.ExpiresAt.Equal(start.Add(lifetime)), "expires_at unchanged inside the interval")

		h.clock.Advance(time.Second) // exactly one interval since the last write
		at := h.clock.Now()
		got, err = h.store.Lookup(ctx, token)
		require.NoError(t, err)
		assert.True(t, got.TouchDue, "after the interval a slide is due")
		slid, err = h.store.Touch(ctx, token, lifetime)
		require.NoError(t, err)
		assert.True(t, slid, "Touch after the interval writes")
		got, err = h.store.Lookup(ctx, token)
		require.NoError(t, err)
		assert.True(t, got.LastSeenAt.Equal(at), "last_seen_at slid to now")
		assert.True(t, got.ExpiresAt.Equal(at.Add(lifetime)), "expires_at slid to now + lifetime")
		assert.True(t, got.CreatedAt.Equal(start), "created_at never moves")
		assert.False(t, got.TouchDue)

		// A second Touch straight after (a concurrent request) is a no-op.
		slid, err = h.store.Touch(ctx, token, lifetime)
		require.NoError(t, err)
		assert.False(t, slid, "only one Touch per interval wins")
	})
}

func TestStore_SlidingKeepsAnActiveSessionAlive(t *testing.T) {
	forEachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		token, _, err := h.store.Create(ctx, h.userA, lifetime, "")
		require.NoError(t, err)

		// Used every 20 days: never 30 days idle, so it never expires.
		for i := 0; i < 4; i++ {
			h.clock.Advance(20 * 24 * time.Hour)
			_, err := h.store.Lookup(ctx, token)
			require.NoError(t, err, "day %d: an active session stays valid", (i+1)*20)
			slid, err := h.store.Touch(ctx, token, lifetime)
			require.NoError(t, err)
			require.True(t, slid)
		}
		// Then idle for the whole lifetime: it expires.
		h.clock.Advance(lifetime)
		_, err = h.store.Lookup(ctx, token)
		assert.True(t, errors.Is(err, session.ErrNotFound))
	})
}

func TestStore_Destroy(t *testing.T) {
	forEachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		keep, _, err := h.store.Create(ctx, h.userA, lifetime, "")
		require.NoError(t, err)
		gone, _, err := h.store.Create(ctx, h.userA, lifetime, "")
		require.NoError(t, err)

		require.NoError(t, h.store.Destroy(ctx, gone))
		_, err = h.store.Lookup(ctx, gone)
		assert.True(t, errors.Is(err, session.ErrNotFound))
		_, err = h.store.Lookup(ctx, keep)
		assert.NoError(t, err, "Destroy removes only the one session")

		assert.NoError(t, h.store.Destroy(ctx, gone), "destroying twice is not an error")
		assert.NoError(t, h.store.Destroy(ctx, "never-existed"))
	})
}

func TestStore_ListForUser(t *testing.T) {
	forEachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		_, old, err := h.store.Create(ctx, h.userA, time.Hour, "old")
		require.NoError(t, err)
		h.clock.Advance(time.Minute)
		_, a1, err := h.store.Create(ctx, h.userA, lifetime, "ipad")
		require.NoError(t, err)
		h.clock.Advance(time.Minute)
		_, a2, err := h.store.Create(ctx, h.userA, lifetime, "laptop")
		require.NoError(t, err)
		_, _, err = h.store.Create(ctx, h.userB, lifetime, "other user")
		require.NoError(t, err)

		h.clock.Advance(time.Hour) // the 1h session is now expired

		list, err := h.store.ListForUser(ctx, h.userA)
		require.NoError(t, err)
		require.Len(t, list, 2, "expired sessions and other users' sessions are excluded")
		assert.Equal(t, a2.Handle, list[0].Handle, "most recently seen first")
		assert.Equal(t, a1.Handle, list[1].Handle)
		for _, s := range list {
			assert.Equal(t, h.userA, s.UserID)
			assert.NotEqual(t, old.Handle, s.Handle)
		}

		none, err := h.store.ListForUser(ctx, "00000000-0000-0000-0000-0000000000ff")
		require.NoError(t, err)
		assert.Empty(t, none)
	})
}

func TestStore_RevokeByHandle(t *testing.T) {
	forEachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		tokA1, a1, err := h.store.Create(ctx, h.userA, lifetime, "")
		require.NoError(t, err)
		tokA2, _, err := h.store.Create(ctx, h.userA, lifetime, "")
		require.NoError(t, err)

		// Another user's id cannot revoke A's session, even with A's handle.
		err = h.store.RevokeByHandle(ctx, h.userB, a1.Handle)
		assert.True(t, errors.Is(err, session.ErrNotFound), "cross-user revoke = %v, want ErrNotFound", err)
		_, err = h.store.Lookup(ctx, tokA1)
		require.NoError(t, err, "a refused cross-user revoke leaves the session alone")

		require.NoError(t, h.store.RevokeByHandle(ctx, h.userA, a1.Handle))
		_, err = h.store.Lookup(ctx, tokA1)
		assert.True(t, errors.Is(err, session.ErrNotFound), "the revoked session is gone")
		_, err = h.store.Lookup(ctx, tokA2)
		assert.NoError(t, err, "revoke removes only the named session")

		for _, bad := range []string{a1.Handle, "", "not-a-handle", "'; DROP TABLE sessions; --"} {
			err = h.store.RevokeByHandle(ctx, h.userA, bad)
			assert.True(t, errors.Is(err, session.ErrNotFound), "RevokeByHandle(%q) = %v, want ErrNotFound", bad, err)
		}
	})
}

func TestStore_DeleteExpired(t *testing.T) {
	forEachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		short1, _, err := h.store.Create(ctx, h.userA, time.Hour, "")
		require.NoError(t, err)
		short2, _, err := h.store.Create(ctx, h.userB, time.Hour, "")
		require.NoError(t, err)
		long, _, err := h.store.Create(ctx, h.userA, lifetime, "")
		require.NoError(t, err)

		n, err := h.store.DeleteExpired(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(0), n, "nothing has expired yet")

		h.clock.Advance(time.Hour)
		n, err = h.store.DeleteExpired(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(2), n, "both 1h sessions are swept")

		for _, tok := range []string{short1, short2} {
			_, err = h.store.Lookup(ctx, tok)
			assert.True(t, errors.Is(err, session.ErrNotFound))
		}
		_, err = h.store.Lookup(ctx, long)
		assert.NoError(t, err, "the sweep leaves live sessions alone")
	})
}

func TestStore_CreateRejectsNonPositiveLifetime(t *testing.T) {
	forEachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		for _, d := range []time.Duration{0, -time.Hour} {
			_, _, err := h.store.Create(ctx, h.userA, d, "")
			assert.Error(t, err, "lifetime %v must be refused", d)
		}
	})
}

// The User-Agent header is attacker-controlled: it may be huge or carry bytes
// that are not valid UTF-8, which Postgres TEXT refuses. Either must be stored
// sanitized — never fail the sign-in.
func TestStore_UserAgentIsSanitized(t *testing.T) {
	forEachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		cases := map[string]string{
			"invalid utf-8": "Mozilla/5.0 \xff\xfe bad",
			"oversized":     strings.Repeat("é", 2000),
			"nul byte":      "a\x00b",
		}
		for name, ua := range cases {
			token, _, err := h.store.Create(ctx, h.userA, lifetime, ua)
			require.NoError(t, err, "%s: Create must not fail on a hostile user agent", name)
			got, err := h.store.Lookup(ctx, token)
			require.NoError(t, err)
			assert.True(t, utf8.ValidString(got.UserAgent), "%s: stored UA is valid UTF-8", name)
			assert.LessOrEqual(t, len(got.UserAgent), 512, "%s: stored UA is capped", name)
			assert.NotContains(t, got.UserAgent, "\x00", "%s: no NUL bytes", name)
		}
	})
}

// Bind seeds a caller-chosen token (integration-test fixtures); it must behave
// exactly like a created session.
func TestStore_Bind(t *testing.T) {
	forEachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		b, ok := h.store.(interface {
			Bind(ctx context.Context, token, userID string, lifetime time.Duration) error
		})
		require.True(t, ok, "both stores expose Bind for fixtures")
		require.NoError(t, b.Bind(ctx, "fixture-token", h.userA, lifetime))
		got, err := h.store.Lookup(ctx, "fixture-token")
		require.NoError(t, err)
		assert.Equal(t, h.userA, got.UserID)
		assert.True(t, got.ExpiresAt.Equal(h.clock.Now().Add(lifetime)))
	})
}
