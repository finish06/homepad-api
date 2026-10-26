// Package session holds homepad's sign-in sessions: an opaque random token in
// an HttpOnly cookie, resolved server-side to a user.
//
// Sessions persist in Postgres (PgStore), so an API restart or deploy no longer
// signs anyone out (SPEC-kid-accounts AC-010). Only sha256(token) is ever
// stored, never the token itself (AC-011). MemoryStore implements the same
// contract for tests.
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned when a token or handle names no live session —
// unknown, destroyed, revoked or expired. Callers treat it as "not signed in".
var ErrNotFound = errors.New("session: not found")

const (
	// AdultLifetime is the sliding lifetime of an admin or user session.
	AdultLifetime = 30 * 24 * time.Hour

	// TouchInterval throttles the slide: last_seen_at and expires_at are written
	// at most once per interval per session, however often the client polls.
	TouchInterval = time.Minute

	// SweepInterval is how often expired rows are deleted.
	SweepInterval = time.Hour

	// maxUserAgentLen caps the stored User-Agent (bytes). The header is
	// client-controlled; it is kept only to label a device.
	maxUserAgentLen = 512
)

// Lifetime returns the sliding session lifetime for a role. Every current role
// (admin, user) is an adult; an unknown role also gets the adult lifetime — the
// shorter one, never the longer. Phase 2 adds the kid role (180 days) here.
func Lifetime(role string) time.Duration {
	return AdultLifetime
}

// Session is one signed-in device. It deliberately has no token field: the
// token is returned once, by Create, and never stored or read back.
type Session struct {
	// Handle is an opaque random id for revoking this session without knowing
	// its token (the Phase 3 device list). It is never derived from the token.
	Handle     string
	UserID     string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	UserAgent  string
	// TouchDue is true when the last write is at least TouchInterval old, so
	// the caller should Touch the session to slide its expiry.
	TouchDue bool
}

// Store is the session persistence contract the HTTP layer depends on.
type Store interface {
	// Create mints a new session for userID that expires after lifetime unless
	// it slides, and returns its token. Only the token's hash is kept.
	Create(ctx context.Context, userID string, lifetime time.Duration, userAgent string) (token string, s Session, err error)
	// Lookup resolves a token to its live session, or ErrNotFound. It never
	// writes; check Session.TouchDue and call Touch to slide.
	Lookup(ctx context.Context, token string) (Session, error)
	// Touch slides a live session: last_seen_at = now, expires_at = now +
	// lifetime. It writes only when the last write is at least TouchInterval
	// old, and reports whether it did (in which case the caller re-issues the
	// cookie). Concurrent Touches within one interval: exactly one wins.
	Touch(ctx context.Context, token string, lifetime time.Duration) (slid bool, err error)
	// Destroy deletes the session for token. Unknown tokens are not an error.
	Destroy(ctx context.Context, token string) error
	// ListForUser returns userID's live sessions, most recently seen first.
	ListForUser(ctx context.Context, userID string) ([]Session, error)
	// RevokeByHandle deletes userID's session with the given handle, or returns
	// ErrNotFound. Scoped to userID so one account cannot revoke another's.
	RevokeByHandle(ctx context.Context, userID, handle string) error
	// DeleteExpired removes every expired session and returns how many.
	DeleteExpired(ctx context.Context) (int64, error)
}

// newToken returns 256 bits of crypto randomness, URL-safe base64 (cookie-safe).
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session: token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// newHandle returns an independent 128-bit random id. It shares no input with
// the token, so a handle reveals nothing about it.
func newHandle() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session: handle: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// hashToken is the only form in which a token is ever stored.
func hashToken(token string) [32]byte {
	return sha256.Sum256([]byte(token))
}

// sanitizeUserAgent makes a client-supplied User-Agent safe to store: no NUL
// bytes or invalid UTF-8 (both refused by Postgres TEXT, which would fail the
// sign-in), capped at maxUserAgentLen bytes on a rune boundary.
func sanitizeUserAgent(ua string) string {
	ua = strings.ReplaceAll(ua, "\x00", "")
	ua = strings.ToValidUTF8(ua, "�")
	if len(ua) > maxUserAgentLen {
		// Cutting may split a multi-byte rune; dropping the invalid tail
		// keeps the result valid UTF-8 and within the cap.
		ua = strings.ToValidUTF8(ua[:maxUserAgentLen], "")
	}
	return ua
}

// clockNow reads an injectable clock, at the microsecond precision Postgres
// timestamptz stores, so both stores report identical times.
func clockNow(clock func() time.Time) time.Time {
	if clock == nil {
		clock = time.Now
	}
	return clock().Truncate(time.Microsecond)
}

func checkLifetime(lifetime time.Duration) error {
	if lifetime <= 0 {
		return fmt.Errorf("session: lifetime must be positive, got %v", lifetime)
	}
	return nil
}

func touchDue(lastSeen, now time.Time) bool {
	return !now.Before(lastSeen.Add(TouchInterval))
}
