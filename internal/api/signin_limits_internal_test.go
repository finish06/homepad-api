package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// SPEC-signin-hardening §2.1, §2.2 — the per-client limiter, the per-email
// login lock, the process ceiling and the client address, on a fake clock.

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
}

// AC-004 — the decided numbers (§2.1, Q3).
func TestDefaultLimits_AreTheSpecNumbers(t *testing.T) {
	want := map[string]RateLimit{
		"login":     {PerMinute: 10, Burst: 10},
		"register":  {PerMinute: 5, Burst: 5},
		"invites":   {PerMinute: 30, Burst: 10},
		"resets":    {PerMinute: 30, Burst: 10},
		"oidcLogin": {PerMinute: 20, Burst: 10},
	}
	got := map[string]RateLimit{
		"login":     DefaultLimits.Login,
		"register":  DefaultLimits.Register,
		"invites":   DefaultLimits.Invite,
		"resets":    DefaultLimits.Reset,
		"oidcLogin": DefaultLimits.OIDCLogin,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("DefaultLimits %s = %+v, want %+v", k, got[k], w)
		}
	}
	// AC-006 — the process ceiling refills at 120 a minute.
	if DefaultLimits.Ceiling.PerMinute != 120 {
		t.Errorf("ceiling = %d a minute, want 120", DefaultLimits.Ceiling.PerMinute)
	}
	if b := DefaultLimits.Ceiling.Burst; b < 1 || b > 120 {
		t.Errorf("ceiling burst = %d, want between 1 and 120", b)
	}
	// Login: 10 failures in 15 minutes lock that email for 15 minutes (Q3).
	if loginFailPolicy != (failurePolicy{max: 10, window: 15 * time.Minute, lockFor: 15 * time.Minute}) {
		t.Errorf("loginFailPolicy = %+v", loginFailPolicy)
	}
}

// A zero field is that route's default; a set one wins.
func TestLimits_WithDefaults(t *testing.T) {
	l := Limits{Login: RateLimit{PerMinute: 1, Burst: 2}}.withDefaults()
	if l.Login != (RateLimit{PerMinute: 1, Burst: 2}) {
		t.Errorf("a set limit is kept, got %+v", l.Login)
	}
	if l.Register != DefaultLimits.Register || l.Ceiling != DefaultLimits.Ceiling || l.OIDCLogin != DefaultLimits.OIDCLogin {
		t.Errorf("zero fields take the defaults, got %+v", l)
	}
}

// AC-004 — each client has its own bucket; one client running dry leaves the
// others alone, and Retry-After is the wait for the next token.
func TestKeyedLimiter_PerClient(t *testing.T) {
	clock := newClock()
	l := newKeyedLimiter("login", RateLimit{PerMinute: 10, Burst: 10}, clock.now)
	for i := 0; i < 10; i++ {
		if ok, _ := l.take("198.51.100.7"); !ok {
			t.Fatalf("request %d is inside the burst", i+1)
		}
	}
	ok, wait := l.take("198.51.100.7")
	if ok {
		t.Fatal("the 11th request in the same instant is over the limit")
	}
	if wait != 6*time.Second {
		t.Fatalf("wait = %v, want 6s (one token every 6s at 10 a minute)", wait)
	}
	if ok, _ := l.take("203.0.113.9"); !ok {
		t.Fatal("another client has its own bucket")
	}
	clock.t = clock.t.Add(6 * time.Second)
	if ok, _ := l.take("198.51.100.7"); !ok {
		t.Fatal("one token refills after 6s")
	}
}

// §2.1 — the map is capped at 10,000 keys and idle keys are dropped after 15
// minutes, so a flood of new addresses can't grow memory without bound.
func TestKeyedLimiter_CapAndIdleDrop(t *testing.T) {
	if limiterMaxKeys != 10_000 || limiterIdleAfter != 15*time.Minute {
		t.Fatalf("limiterMaxKeys=%d limiterIdleAfter=%v, want 10000 and 15m", limiterMaxKeys, limiterIdleAfter)
	}
	clock := newClock()
	l := newKeyedLimiter("register", RateLimit{PerMinute: 5, Burst: 5}, clock.now)
	for i := 0; i < limiterMaxKeys+50; i++ {
		l.take("client-" + strconv.Itoa(i))
	}
	if n := l.size(); n > limiterMaxKeys {
		t.Fatalf("map holds %d keys, want at most %d", n, limiterMaxKeys)
	}
	// The newest key is kept: a full map never refuses a new client.
	if ok, _ := l.take("client-" + strconv.Itoa(limiterMaxKeys+49)); !ok {
		t.Fatal("the newest client still has tokens")
	}

	clock.t = clock.t.Add(limiterIdleAfter + time.Second)
	l.take("late")
	if n := l.size(); n != 1 {
		t.Fatalf("after 15 idle minutes the map holds %d keys, want only the new one", n)
	}
}

// A client that ran dry and is evicted while busy is not the common case,
// but eviction picks the least recently seen key, never the busiest.
func TestKeyedLimiter_EvictsTheOldest(t *testing.T) {
	clock := newClock()
	l := newKeyedLimiter("login", RateLimit{PerMinute: 10, Burst: 1}, clock.now)
	l.take("busy")
	for i := 1; i < limiterMaxKeys; i++ {
		clock.t = clock.t.Add(time.Microsecond)
		l.take("k" + strconv.Itoa(i))
	}
	clock.t = clock.t.Add(time.Microsecond)
	l.take("busy") // seen again: now the newest
	clock.t = clock.t.Add(time.Microsecond)
	l.take("one-more")
	if ok, _ := l.take("busy"); ok {
		t.Fatal("busy kept its empty bucket: it was not the oldest key")
	}
}

// Concurrent takes never hand out more than the burst.
func TestKeyedLimiter_Concurrent(t *testing.T) {
	l := newKeyedLimiter("login", RateLimit{PerMinute: 1, Burst: 10}, newClock().now)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.take("same"); ok {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != 10 {
		t.Fatalf("granted %d, want exactly the burst of 10", granted)
	}
}

// AC-005 — 10 failures in 15 minutes lock that email for 15 minutes; a
// success before the tenth clears the count; other emails are unaffected.
func TestLoginFails_LockAfterTen(t *testing.T) {
	clock := newClock()
	f := newFailureLimiter(clock.now, loginFailPolicy)
	key := loginFailKey("jo@example.com")
	for i := 0; i < 9; i++ {
		f.fail(key)
		if f.locked(key) != 0 {
			t.Fatalf("locked after %d failures, want only at 10", i+1)
		}
	}
	f.fail(key)
	wait := f.locked(key)
	if wait != 15*time.Minute {
		t.Fatalf("after 10 failures the lock is %v, want 15m", wait)
	}
	if f.locked(loginFailKey("caleb@example.com")) != 0 {
		t.Fatal("another email is not locked")
	}
	clock.t = clock.t.Add(15*time.Minute - time.Second)
	if f.locked(key) != time.Second {
		t.Fatalf("lock left = %v, want 1s", f.locked(key))
	}
	clock.t = clock.t.Add(time.Second)
	if f.locked(key) != 0 {
		t.Fatal("the lock ends after 15 minutes")
	}
}

func TestLoginFails_SuccessClearsAndWindowSlides(t *testing.T) {
	clock := newClock()
	f := newFailureLimiter(clock.now, loginFailPolicy)
	key := loginFailKey("jo@example.com")
	for i := 0; i < 9; i++ {
		f.fail(key)
	}
	f.clear(key) // a right password
	for i := 0; i < 9; i++ {
		f.fail(key)
	}
	if f.locked(key) != 0 {
		t.Fatal("a success before the tenth failure clears the count")
	}
	// Failures older than 15 minutes don't count.
	clock.t = clock.t.Add(15 * time.Minute)
	f.fail(key)
	if f.locked(key) != 0 {
		t.Fatal("failures outside the 15-minute window don't count")
	}
}

// The lock key is the email, folded to lower case and hashed: the email is
// never kept in memory or logged, and Jo@Example.com is the same account.
func TestLoginFailKey_FoldsAndHides(t *testing.T) {
	a, b := loginFailKey(" Jo@Example.com "), loginFailKey("jo@example.com")
	if a != b {
		t.Fatal("case and spaces fold to one key")
	}
	if strings.Contains(a, "jo") || strings.Contains(a, "example") {
		t.Fatalf("key %q carries the email", a)
	}
	if loginFailKey("pat@example.com") == b {
		t.Fatal("different emails have different keys")
	}
}

// The failure map is capped like the limiter's.
func TestFailureLimiter_Capped(t *testing.T) {
	clock := newClock()
	f := newFailureLimiter(clock.now, loginFailPolicy)
	for i := 0; i < limiterMaxKeys+20; i++ {
		f.fail(loginFailKey("p" + strconv.Itoa(i) + "@example.com"))
	}
	if n := f.size(); n > limiterMaxKeys {
		t.Fatalf("failure map holds %d keys, want at most %d", n, limiterMaxKeys)
	}
}

// A full failure map evicts an unlocked entry before a locked one, so a
// flood of new emails can't lift a lock.
func TestFailureLimiter_KeepsLocksWhenFull(t *testing.T) {
	clock := newClock()
	f := newFailureLimiter(clock.now, loginFailPolicy)
	key := loginFailKey("jo@example.com")
	for i := 0; i < 10; i++ {
		f.fail(key)
	}
	for i := 0; i < limiterMaxKeys+20; i++ {
		clock.t = clock.t.Add(time.Microsecond)
		f.fail(loginFailKey("p" + strconv.Itoa(i) + "@example.com"))
	}
	if f.locked(key) == 0 {
		t.Fatal("a flood of other emails lifted the lock")
	}
}

// A lock is logged at most once a minute per email.
func TestFailureLimiter_LockedLogThrottle(t *testing.T) {
	clock := newClock()
	f := newFailureLimiter(clock.now, loginFailPolicy)
	key := loginFailKey("jo@example.com")
	for i := 0; i < 10; i++ {
		f.fail(key)
	}
	if _, logIt := f.lockedLog(key); !logIt {
		t.Fatal("the first locked try is logged")
	}
	if _, logIt := f.lockedLog(key); logIt {
		t.Fatal("not again inside the minute")
	}
	clock.t = clock.t.Add(time.Minute)
	if wait, logIt := f.lockedLog(key); !logIt || wait <= 0 {
		t.Fatalf("a minute later: wait=%v logIt=%v", wait, logIt)
	}
	if wait, logIt := f.lockedLog(loginFailKey("nobody@example.com")); wait != 0 || logIt {
		t.Fatal("an unlocked email reports nothing")
	}
}

// --- client address (§2.2, AC-007, AC-008) --------------------------------

func fixedResolver(m map[string][]string) func(context.Context, string) ([]netip.Addr, error) {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		addrs, ok := m[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		var out []netip.Addr
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
}

func clientOf(t *testing.T, tp *TrustedProxies, remote string, xff ...string) string {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/login", nil)
	r.RemoteAddr = remote
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	r.Header.Set("X-Real-IP", "192.0.2.250")
	return clientAddr(r, tp)
}

// AC-007 — unset, the client is the TCP peer; X-Forwarded-For and X-Real-IP
// are ignored.
func TestClientAddr_UnsetIgnoresHeaders(t *testing.T) {
	tp, err := ParseTrustedProxies("")
	if err != nil {
		t.Fatal(err)
	}
	if got := clientOf(t, tp, "172.18.0.3:51234", "198.51.100.7"); got != "172.18.0.3" {
		t.Errorf("client = %q, want the peer 172.18.0.3", got)
	}
	if got := clientOf(t, nil, "172.18.0.3:51234", "198.51.100.7"); got != "172.18.0.3" {
		t.Errorf("nil proxies: client = %q, want the peer", got)
	}
	if got := clientOf(t, tp, "[2001:db8::5]:443"); got != "2001:db8::5" {
		t.Errorf("IPv6 peer = %q", got)
	}
	if got := clientOf(t, tp, "[::ffff:10.0.0.4]:443"); got != "10.0.0.4" {
		t.Errorf("IPv4-mapped peer = %q, want 10.0.0.4", got)
	}
}

// AC-008 — a trusted peer: the right-most entry that isn't trusted.
func TestClientAddr_TrustedPeer(t *testing.T) {
	tp, err := ParseTrustedProxies("172.18.0.0/16, 10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"one hop", "172.18.0.3:5000", []string{"198.51.100.7"}, "198.51.100.7"},
		{"forged left entry", "172.18.0.3:5000", []string{"6.6.6.6, 198.51.100.7"}, "198.51.100.7"},
		{"chain of trusted hops", "172.18.0.3:5000", []string{"198.51.100.7, 10.0.0.1"}, "198.51.100.7"},
		{"split header lines", "172.18.0.3:5000", []string{"6.6.6.6", "198.51.100.7, 10.0.0.1"}, "198.51.100.7"},
		{"no header", "172.18.0.3:5000", nil, "172.18.0.3"},
		{"only trusted hops", "172.18.0.3:5000", []string{"10.0.0.1"}, "10.0.0.1"},
		{"garbage right-most", "172.18.0.3:5000", []string{"198.51.100.7, not-an-ip"}, "172.18.0.3"},
		{"entry with a port", "172.18.0.3:5000", []string{"198.51.100.7:4711"}, "198.51.100.7"},
		{"bracketed IPv6", "172.18.0.3:5000", []string{"[2001:db8::7]:4711"}, "2001:db8::7"},
		{"untrusted peer ignores header", "203.0.113.5:5000", []string{"198.51.100.7"}, "203.0.113.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clientOf(t, tp, c.remote, c.xff...); got != c.want {
				t.Errorf("client = %q, want %q", got, c.want)
			}
		})
	}
}

// AC-008 — an unparsable value is an error that names the variable.
func TestParseTrustedProxies_Errors(t *testing.T) {
	for _, raw := range []string{"10.0.0.0/33", "300.1.2.3", "proxy one", "http://proxy", "10.0.0.1/8/2", "-bad-", "a..b"} {
		_, err := ParseTrustedProxies(raw)
		if err == nil {
			t.Errorf("%q: want an error", raw)
			continue
		}
		if !strings.Contains(err.Error(), "HOMEPAD_TRUSTED_PROXIES") {
			t.Errorf("%q: error %q does not name HOMEPAD_TRUSTED_PROXIES", raw, err)
		}
	}
	for _, raw := range []string{"", "  ", "10.0.0.1", "10.0.0.0/8", "2001:db8::/32", "proxy", "web.homepad_default", "nginx, 10.0.0.1 ,192.168.0.0/16", "10.0.0.1,"} {
		if _, err := ParseTrustedProxies(raw); err != nil {
			t.Errorf("%q: unexpected error %v", raw, err)
		}
	}
}

// Hostnames resolve at startup and on every refresh; until a name resolves,
// nothing it names is trusted.
func TestTrustedProxies_Hostnames(t *testing.T) {
	addrs := map[string][]string{"web": {"172.18.0.3"}}
	tp, err := parseTrustedProxies("web, 10.0.0.1", fixedResolver(addrs))
	if err != nil {
		t.Fatal(err)
	}
	if !tp.HasHostnames() {
		t.Fatal("web is a hostname")
	}
	tp.Refresh(context.Background())
	if got := clientOf(t, tp, "172.18.0.3:1", "198.51.100.7"); got != "198.51.100.7" {
		t.Fatalf("resolved web is trusted: client = %q", got)
	}
	// The container moves; the next refresh follows it.
	addrs["web"] = []string{"172.18.0.9"}
	tp.Refresh(context.Background())
	if got := clientOf(t, tp, "172.18.0.3:1", "198.51.100.7"); got != "172.18.0.3" {
		t.Fatalf("the old address is no longer trusted: client = %q", got)
	}
	if got := clientOf(t, tp, "172.18.0.9:1", "198.51.100.7"); got != "198.51.100.7" {
		t.Fatalf("the new address is trusted: client = %q", got)
	}
	// A failed lookup keeps the last good answer.
	delete(addrs, "web")
	tp.Refresh(context.Background())
	if got := clientOf(t, tp, "172.18.0.9:1", "198.51.100.7"); got != "198.51.100.7" {
		t.Fatalf("a failed refresh keeps the last addresses: client = %q", got)
	}

	plain, _ := parseTrustedProxies("10.0.0.1", fixedResolver(nil))
	if plain.HasHostnames() {
		t.Fatal("an address list has no hostnames")
	}
}

func TestTrustedProxies_RunRefreshesUntilDone(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	tp, err := parseTrustedProxies("web", func(context.Context, string) ([]netip.Addr, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return []netip.Addr{netip.MustParseAddr("172.18.0.3")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tp.Run(ctx, time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if calls < 3 {
		t.Fatalf("resolved %d times, want a refresh on every tick", calls)
	}
	if trustedRefreshEvery != 5*time.Minute {
		t.Fatalf("trustedRefreshEvery = %v, want 5m (§2.2)", trustedRefreshEvery)
	}
}

// --- AC-009, by the clock ---------------------------------------------------

// The median login time for an unknown email and for a known email with a
// wrong password differ by under 20%. Both run one bcrypt compare at
// DefaultCost (TestLoginPasswordOK_RunsOneCompareOnEveryPath counts them);
// this checks the time too.
func TestLogin_TimingMediansMatch(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("the real password"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	known := storage.User{ID: "u1", Role: "user", PasswordHash: string(hash)}
	s := &server{}
	const n = 9
	median := func(f func()) time.Duration {
		ds := make([]time.Duration, n)
		for i := range ds {
			start := time.Now()
			f()
			ds[i] = time.Since(start)
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[n/2]
	}
	unknown := median(func() { s.loginPasswordOK(storage.User{}, storage.ErrNotFound, "a wrong password") })
	wrong := median(func() { s.loginPasswordOK(known, nil, "a wrong password") })
	hi, lo := unknown, wrong
	if lo > hi {
		hi, lo = lo, hi
	}
	if float64(hi-lo)/float64(hi) >= 0.20 {
		t.Fatalf("median unknown=%v wrong-password=%v differ by %.0f%%, want under 20%%",
			unknown, wrong, 100*float64(hi-lo)/float64(hi))
	}
}
