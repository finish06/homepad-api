package api

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimit is a token bucket's refill rate and size (SPEC-household-basics
// §3.4). The zero value means DefaultInviteLimit.
type RateLimit struct {
	PerMinute int
	Burst     int
}

// DefaultInviteLimit is the one bucket POST /api/invites/lookup and
// /api/invites/accept share: 30 requests a minute, burst 10 (AC-017). Accept
// runs bcrypt, so this bounds CPU on a public route. It is per process, not
// per IP: behind the web container's nginx every request has the same peer.
var DefaultInviteLimit = RateLimit{PerMinute: 30, Burst: 10}

// tokenBucket is a mutex-guarded token bucket on an injectable clock.
type tokenBucket struct {
	mu     sync.Mutex
	now    func() time.Time
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

func newTokenBucket(l RateLimit, now func() time.Time) *tokenBucket {
	if l.PerMinute <= 0 || l.Burst <= 0 {
		l = DefaultInviteLimit
	}
	if now == nil {
		now = time.Now
	}
	return &tokenBucket{
		now:    now,
		rate:   float64(l.PerMinute) / 60,
		burst:  float64(l.Burst),
		tokens: float64(l.Burst),
		last:   now(),
	}
}

// take spends one token. When none is left it reports how long until one is.
func (b *tokenBucket) take() (ok bool, retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(b.burst, b.tokens+elapsed*b.rate)
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := (1 - b.tokens) / b.rate
	return false, time.Duration(math.Ceil(wait*1e9)) * time.Nanosecond
}

// retryAfterSeconds renders a wait as a Retry-After value: whole seconds,
// rounded up, at least 1.
func retryAfterSeconds(d time.Duration) string {
	s := int(math.Ceil(d.Seconds()))
	if s < 1 {
		s = 1
	}
	return strconv.Itoa(s)
}

// rateLimited wraps a public handler with the bucket: over the limit it
// answers 429 with Retry-After and never calls h.
func rateLimited(b *tokenBucket, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := b.take(); !ok {
			w.Header().Set("Retry-After", retryAfterSeconds(wait))
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		h(w, r)
	}
}
