package api

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimit is a token bucket's refill rate and size. The zero value means
// the route's default.
type RateLimit struct {
	PerMinute int
	Burst     int
}

// DefaultInviteLimit is the per-client limit on POST /api/invites/lookup and
// /accept: 30 requests a minute, burst 10 (SPEC-household-basics AC-017).
// Reset links use the same numbers.
var DefaultInviteLimit = RateLimit{PerMinute: 30, Burst: 10}

// Limits are the sign-in limits (SPEC-signin-hardening §2.1). Each per-client
// limit is a bucket per client key (clientAddr); Ceiling is one bucket for
// the whole process, shared by the routes that run bcrypt, so their CPU cost
// is bounded whatever the client keys are. The cost is availability: a few
// busy clients can empty it, and then every password sign-in waits. A zero
// field is its DefaultLimits value.
type Limits struct {
	Login     RateLimit // POST /api/login
	Register  RateLimit // POST /api/register
	Invite    RateLimit // POST /api/invites/lookup and /accept, one bucket
	Reset     RateLimit // POST /api/password-resets/lookup and /accept, one bucket
	OIDCLogin RateLimit // GET /api/auth/oidc/login
	Ceiling   RateLimit // process-wide: login, register and the two accept routes
}

// DefaultLimits are the decided numbers (§2.1, Q3). The ceiling refills at
// 120 a minute; its burst of 20 bounds a spike to about 20 bcrypt compares.
var DefaultLimits = Limits{
	Login:     RateLimit{PerMinute: 10, Burst: 10},
	Register:  RateLimit{PerMinute: 5, Burst: 5},
	Invite:    DefaultInviteLimit,
	Reset:     DefaultInviteLimit,
	OIDCLogin: RateLimit{PerMinute: 20, Burst: 10},
	Ceiling:   RateLimit{PerMinute: 120, Burst: 20},
}

func (l Limits) withDefaults() Limits {
	pick := func(v, def RateLimit) RateLimit {
		if v.PerMinute <= 0 || v.Burst <= 0 {
			return def
		}
		return v
	}
	d := DefaultLimits
	return Limits{
		Login:     pick(l.Login, d.Login),
		Register:  pick(l.Register, d.Register),
		Invite:    pick(l.Invite, d.Invite),
		Reset:     pick(l.Reset, d.Reset),
		OIDCLogin: pick(l.OIDCLogin, d.OIDCLogin),
		Ceiling:   pick(l.Ceiling, d.Ceiling),
	}
}

// Every in-memory limit map is capped, and forgets idle keys (§2.1). A key
// idle for limiterIdleAfter has refilled to its burst anyway, so dropping it
// loses nothing.
const (
	limiterMaxKeys   = 10_000
	limiterIdleAfter = 15 * time.Minute
	// limiterLogEvery throttles each key's audit line (§3).
	limiterLogEvery = time.Minute
	// limiterSweepEvery is how often a map is swept for idle keys.
	limiterSweepEvery = time.Minute
)

// tokenBucket is a mutex-guarded token bucket on an injectable clock: the
// process ceiling.
type tokenBucket struct {
	mu     sync.Mutex
	now    func() time.Time
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
	logged time.Time
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

// refill spends one token from a bucket holding tokens at last, refilling at
// rate up to burst. It returns the new count and, when there was no token,
// how long until one is.
func refill(tokens float64, last, now time.Time, rate, burst float64) (left float64, ok bool, wait time.Duration) {
	if elapsed := now.Sub(last).Seconds(); elapsed > 0 {
		tokens = math.Min(burst, tokens+elapsed*rate)
	}
	if tokens >= 1 {
		return tokens - 1, true, 0
	}
	w := (1 - tokens) / rate
	return tokens, false, time.Duration(math.Ceil(w*1e9)) * time.Nanosecond
}

// take spends one token. When none is left it reports how long until one is.
func (b *tokenBucket) take() (ok bool, retryAfter time.Duration) {
	ok, retryAfter, _ = b.takeLog()
	return ok, retryAfter
}

// takeLog is take, also reporting whether a refusal should be logged (at
// most once per limiterLogEvery).
func (b *tokenBucket) takeLog() (ok bool, retryAfter time.Duration, logIt bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.tokens, ok, retryAfter = refill(b.tokens, b.last, now, b.rate, b.burst)
	b.last = now
	if !ok && (b.logged.IsZero() || now.Sub(b.logged) >= limiterLogEvery) {
		b.logged = now
		logIt = true
	}
	return ok, retryAfter, logIt
}

// keyedLimiter is one token bucket per key (a client address), in a map
// capped at limiterMaxKeys. It is per process: a restart clears it, and two
// API replicas each keep their own.
type keyedLimiter struct {
	name  string
	mu    sync.Mutex
	now   func() time.Time
	rate  float64
	burst float64
	byKey map[string]*keyedBucket
	swept time.Time
}

type keyedBucket struct {
	tokens float64
	last   time.Time
	logged time.Time
}

func newKeyedLimiter(name string, l RateLimit, now func() time.Time) *keyedLimiter {
	if now == nil {
		now = time.Now
	}
	return &keyedLimiter{name: name, now: now, rate: float64(l.PerMinute) / 60, burst: float64(l.Burst),
		byKey: map[string]*keyedBucket{}}
}

// take spends one of key's tokens. When none is left it reports how long
// until one is.
func (l *keyedLimiter) take(key string) (ok bool, wait time.Duration) {
	ok, wait, _ = l.takeLog(key)
	return ok, wait
}

// takeLog is take, also reporting whether a refusal should be logged (at
// most once per limiterLogEvery for each key).
func (l *keyedLimiter) takeLog(key string) (ok bool, wait time.Duration, logIt bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.swept) >= limiterSweepEvery {
		l.sweep(now)
	}
	b := l.byKey[key]
	if b == nil {
		if len(l.byKey) >= limiterMaxKeys {
			l.sweep(now)
			if len(l.byKey) >= limiterMaxKeys {
				l.evictOldest()
			}
		}
		b = &keyedBucket{tokens: l.burst, last: now}
		l.byKey[key] = b
	}
	b.tokens, ok, wait = refill(b.tokens, b.last, now, l.rate, l.burst)
	b.last = now
	if !ok && (b.logged.IsZero() || now.Sub(b.logged) >= limiterLogEvery) {
		b.logged = now
		logIt = true
	}
	return ok, wait, logIt
}

// sweep drops every key idle for limiterIdleAfter. The caller holds mu.
func (l *keyedLimiter) sweep(now time.Time) {
	l.swept = now
	for k, b := range l.byKey {
		if now.Sub(b.last) >= limiterIdleAfter {
			delete(l.byKey, k)
		}
	}
}

// evictOldest drops the least recently seen key. The caller holds mu.
func (l *keyedLimiter) evictOldest() {
	var oldest string
	var at time.Time
	for k, b := range l.byKey {
		if oldest == "" || b.last.Before(at) {
			oldest, at = k, b.last
		}
	}
	delete(l.byKey, oldest)
}

func (l *keyedLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.byKey)
}

// failurePolicy is a failure lock: max failures within window lock the key
// for lockFor.
type failurePolicy struct {
	max     int
	window  time.Duration
	lockFor time.Duration
}

// loginFailPolicy is the per-email login lock (SPEC-signin-hardening AC-005,
// Q3): 10 failures in 15 minutes lock that email for 15 minutes.
var loginFailPolicy = failurePolicy{max: 10, window: 15 * time.Minute, lockFor: 15 * time.Minute}

// failureLimiter counts failures per key (an account id, or a hashed email)
// and locks a key at its policy's max. It is per process, capped at
// limiterMaxKeys.
type failureLimiter struct {
	mu     sync.Mutex
	now    func() time.Time
	policy failurePolicy
	byKey  map[string]*failures
	swept  time.Time
}

type failures struct {
	at          []time.Time // failures inside the window, oldest first
	lockedUntil time.Time
	loggedAt    time.Time // last fail() audit line
	lockLogged  time.Time // last lockedLog() audit line
}

func newFailureLimiter(now func() time.Time, p failurePolicy) *failureLimiter {
	if now == nil {
		now = time.Now
	}
	return &failureLimiter{now: now, policy: p, byKey: map[string]*failures{}}
}

// locked returns how long key is still locked out, or 0.
func (l *failureLimiter) locked(key string) time.Duration {
	wait, _ := l.check(key, false)
	return wait
}

// lockedLog is locked, also reporting whether to log the lock: at most once
// per limiterLogEvery for each key.
func (l *failureLimiter) lockedLog(key string) (wait time.Duration, logIt bool) {
	return l.check(key, true)
}

func (l *failureLimiter) check(key string, forLog bool) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.byKey[key]
	if f == nil {
		return 0, false
	}
	now := l.now()
	wait := f.lockedUntil.Sub(now)
	if wait <= 0 {
		return 0, false
	}
	if forLog && (f.lockLogged.IsZero() || now.Sub(f.lockLogged) >= limiterLogEvery) {
		f.lockLogged = now
		return wait, true
	}
	return wait, false
}

// fail records one failure for key, locking it at the policy's max inside
// the window. It reports whether this failure should be logged (at most one
// line per limiterLogEvery).
func (l *failureLimiter) fail(key string) (logIt bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.swept) >= limiterSweepEvery {
		l.sweep(now)
	}
	f := l.byKey[key]
	if f == nil {
		if len(l.byKey) >= limiterMaxKeys {
			l.sweep(now)
			if len(l.byKey) >= limiterMaxKeys {
				l.evict(now)
			}
		}
		f = &failures{}
		l.byKey[key] = f
	}
	kept := f.at[:0]
	for _, t := range f.at {
		if now.Sub(t) < l.policy.window {
			kept = append(kept, t)
		}
	}
	f.at = append(kept, now)
	if len(f.at) >= l.policy.max {
		f.lockedUntil = now.Add(l.policy.lockFor)
		f.at = nil
	}
	if f.loggedAt.IsZero() || now.Sub(f.loggedAt) >= limiterLogEvery {
		f.loggedAt = now
		return true
	}
	return false
}

// clear forgets key's failures after a success.
func (l *failureLimiter) clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byKey, key)
}

// sweep drops keys with no lock and no failure inside the window. The
// caller holds mu.
func (l *failureLimiter) sweep(now time.Time) {
	l.swept = now
	for k, f := range l.byKey {
		if !f.lockedUntil.After(now) && (len(f.at) == 0 || now.Sub(f.at[len(f.at)-1]) >= l.policy.window) {
			delete(l.byKey, k)
		}
	}
}

// evict makes room in a full map: the unlocked key with the oldest last
// failure goes first, so a flood of new keys can't lift a lock. Only when
// every key is locked does the lock that ends soonest go. The caller holds mu.
func (l *failureLimiter) evict(now time.Time) {
	var victim string
	var victimLocked bool
	var at time.Time
	for k, f := range l.byKey {
		isLocked := f.lockedUntil.After(now)
		when := f.lockedUntil
		if !isLocked && len(f.at) > 0 {
			when = f.at[len(f.at)-1]
		}
		better := victim == "" ||
			(victimLocked && !isLocked) ||
			(victimLocked == isLocked && when.Before(at))
		if better {
			victim, victimLocked, at = k, isLocked, when
		}
	}
	delete(l.byKey, victim)
}

func (l *failureLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.byKey)
}

// loginFailKey is the per-email lock's key: the email folded to lower case
// and hashed, so the lock map never holds an email (§3, Logging).
func loginFailKey(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(sum[:])
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

// tooMany is every sign-in limit's answer (§2.1): 429, Retry-After in whole
// seconds, and a body that doesn't say which limit it was.
func tooMany(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", retryAfterSeconds(wait))
	http.Error(w, "too many attempts", http.StatusTooManyRequests)
}

// limited wraps a public bcrypt route with its per-client bucket and then
// the process ceiling. Over either it answers tooMany and never calls h.
// route names the route in the audit line.
func (s *server) limited(route string, kl *keyedLimiter, h http.HandlerFunc) http.HandlerFunc {
	return s.perClient(route, kl, func(w http.ResponseWriter, r *http.Request) {
		if ok, wait, logIt := s.ceiling.takeLog(); !ok {
			if logIt {
				log.Printf("ratelimit: %s limited by the process ceiling", route)
			}
			tooMany(w, wait)
			return
		}
		h(w, r)
	})
}

// perClient wraps a public route with its per-client bucket only. The
// invite and reset lookups use it: they run no bcrypt (one SHA-256 and one
// indexed read), so they don't spend the process ceiling.
func (s *server) perClient(route string, kl *keyedLimiter, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		client := clientAddr(r, s.trusted)
		if ok, wait, logIt := kl.takeLog(client); !ok {
			if logIt {
				log.Printf("ratelimit: %s limited client=%s", route, client)
			}
			tooMany(w, wait)
			return
		}
		h(w, r)
	}
}

// oidcStartLimited wraps GET /api/auth/oidc/login with its per-client
// bucket. The browser reaches it by full-page navigation, so over the limit
// it lands on the sign-in screen with too_many instead of a 429 text page
// (AC-019, UX S-5).
func (s *server) oidcStartLimited(kl *keyedLimiter, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		client := clientAddr(r, s.trusted)
		if ok, _, logIt := kl.takeLog(client); !ok {
			if logIt {
				log.Printf("ratelimit: GET /api/auth/oidc/login limited client=%s", client)
			}
			signinError(w, r, signinTooMany)
			return
		}
		h(w, r)
	}
}
