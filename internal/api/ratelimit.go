package api

import "time"

// RateLimit is a token bucket's refill rate and size (SPEC-household-basics
// §3.4). The zero value means DefaultInviteLimit.
type RateLimit struct {
	PerMinute int
	Burst     int
}

// DefaultInviteLimit is the public invite routes' shared limit.
var DefaultInviteLimit = RateLimit{}

type tokenBucket struct{}

func newTokenBucket(l RateLimit, now func() time.Time) *tokenBucket { return &tokenBucket{} }

func (b *tokenBucket) take() (bool, time.Duration) { return true, 0 }

func retryAfterSeconds(d time.Duration) string { return "" }
