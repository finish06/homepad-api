package api

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// The OIDC placeholder an SSO account stores is the one storage treats as
// "no local password" (AC-020, AC-021), so the two can't drift apart.
func TestHasLocalPassword_KnowsTheOIDCPlaceholder(t *testing.T) {
	assert.False(t, storage.User{Role: "user", PasswordHash: oidcNoPassword}.HasLocalPassword())
	assert.False(t, storage.User{Role: "admin", PasswordHash: oidcNoPassword}.HasLocalPassword())
	assert.False(t, storage.User{Role: "kid"}.HasLocalPassword())
	assert.False(t, storage.User{Role: "user"}.HasLocalPassword())
	assert.True(t, storage.User{Role: "user", PasswordHash: dummyPasswordHash}.HasLocalPassword())
	assert.True(t, storage.User{Role: "admin", PasswordHash: dummyPasswordHash}.HasLocalPassword())
}

type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// §3.3 — 5 wrong tries inside 15 minutes lock the account for 15 minutes;
// tries spread wider than the window never do; a right try clears the count.
func TestFailureLimiter(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
	l := newFailureLimiter(clk.now, passwordFailPolicy)

	for i := 0; i < 4; i++ {
		l.fail("a")
		clk.advance(time.Minute)
	}
	assert.Zero(t, l.locked("a"), "four tries don't lock")
	l.fail("a")
	assert.Equal(t, passwordLockFor, l.locked("a"), "the fifth locks for 15 minutes")
	assert.Zero(t, l.locked("b"), "per account")

	clk.advance(10 * time.Minute)
	assert.Equal(t, 5*time.Minute, l.locked("a"))
	clk.advance(5 * time.Minute)
	assert.Zero(t, l.locked("a"), "the lock ends")

	// Spread out: one try every 4 minutes never has 5 inside 15 minutes.
	for i := 0; i < 10; i++ {
		l.fail("c")
		clk.advance(4 * time.Minute)
	}
	assert.Zero(t, l.locked("c"))

	for i := 0; i < 4; i++ {
		l.fail("d")
	}
	l.clear("d")
	l.fail("d")
	assert.Zero(t, l.locked("d"), "a right password clears the count")
}

// §3.4 — the wrong-password line is logged at most once a minute per account.
func TestFailureLimiter_LogsOnceAMinute(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
	l := newFailureLimiter(clk.now, passwordFailPolicy)
	assert.True(t, l.fail("a"))
	clk.advance(30 * time.Second)
	assert.False(t, l.fail("a"))
	assert.True(t, l.fail("b"), "per account")
	clk.advance(30 * time.Second)
	assert.True(t, l.fail("a"))
	assert.NotNil(t, newFailureLimiter(nil, passwordFailPolicy).now, "nil clock means time.Now")
}
