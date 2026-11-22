package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-signin-hardening §2.1, §2.2 — the sign-in limits over HTTP. Every
// request in a test comes from 127.0.0.1, the httptest peer.

// roomyClient keeps the per-client limit out of the way of a test about the
// per-email lock or the ceiling.
var roomyClient = api.RateLimit{PerMinute: 6000, Burst: 1000}

// postFrom POSTs body with the given X-Forwarded-For ("" sends none).
func postFrom(t *testing.T, url, xff string, body any) (int, http.Header, string) {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, string(readAll(t, resp))
}

func login(t *testing.T, env *testsupport.Env, xff, email, password string) (int, http.Header, string) {
	t.Helper()
	return postFrom(t, env.URL+"/api/login", xff, map[string]string{"email": email, "password": password})
}

func requireTooMany(t *testing.T, code int, h http.Header, body string) int {
	t.Helper()
	require.Equal(t, http.StatusTooManyRequests, code, body)
	assert.Equal(t, "too many attempts\n", body)
	retry, err := strconv.Atoi(h.Get("Retry-After"))
	require.NoError(t, err, "Retry-After is whole seconds")
	assert.GreaterOrEqual(t, retry, 1)
	return retry
}

// AC-004, AC-007, TC-002 — login: 10 a minute per client, burst 10. With
// HOMEPAD_TRUSTED_PROXIES unset, a different forged X-Forwarded-For on every
// try changes nothing: the 11th is 429.
//
// The first ten tries are refused before bcrypt (a password under 8
// characters), so they take no time and no token refills before the 11th.
func TestLogin_PerClientLimit_IgnoresForgedHeader(t *testing.T) {
	env := testsupport.NewEnv(t)
	for i := 0; i < 10; i++ {
		code, _, body := login(t, env, fmt.Sprintf("198.51.100.%d", i+1), fmt.Sprintf("nobody%d@example.com", i), "short")
		require.Equal(t, http.StatusBadRequest, code, "try %d: %s", i+1, body)
	}
	code, h, body := login(t, env, "198.51.100.99", "alice@example.com", "correct horse battery staple")
	requireTooMany(t, code, h, body)
}

// AC-004 — under the limit, nothing changes.
func TestLogin_UnderTheLimitIsUnchanged(t *testing.T) {
	env := testsupport.NewEnv(t)
	code, _, body := login(t, env, "", "alice@example.com", "correct horse battery staple")
	require.Equal(t, http.StatusOK, code, body)
	code, _, _ = login(t, env, "", "alice@example.com", "wrong password")
	require.Equal(t, http.StatusUnauthorized, code)
}

// AC-004 — register: 5 a minute per client, burst 5.
func TestRegister_PerClientLimit(t *testing.T) {
	env := testsupport.NewEnv(t)
	for i := 0; i < 5; i++ {
		code, _, body := postFrom(t, env.URL+"/api/register", "", map[string]string{
			"email": fmt.Sprintf("new%d@example.com", i), "password": "a long enough password"})
		require.Equal(t, http.StatusCreated, code, "register %d: %s", i+1, body)
	}
	code, h, body := postFrom(t, env.URL+"/api/register", "", map[string]string{"email": "sixth@example.com", "password": "a long enough password"})
	requireTooMany(t, code, h, body)
}

// AC-004 — invites and reset links: 30 a minute per client, burst 10, one
// bucket for each pair of routes.
func TestInviteAndResetRoutes_PerClientLimit(t *testing.T) {
	for _, pair := range [][2]string{
		{"/api/invites/lookup", "/api/invites/accept"},
		{"/api/password-resets/lookup", "/api/password-resets/accept"},
	} {
		t.Run(pair[0], func(t *testing.T) {
			env := testsupport.NewEnv(t)
			token := strings.Repeat("A", 43)
			for i := 0; i < 10; i++ {
				code, _, body := postFrom(t, env.URL+pair[0], "", map[string]string{"token": token})
				require.Equal(t, http.StatusNotFound, code, "try %d: %s", i+1, body)
			}
			code, h, body := postFrom(t, env.URL+pair[0], "", map[string]string{"token": token})
			requireTooMany(t, code, h, body)
			code, h, body = postFrom(t, env.URL+pair[1], "", map[string]string{"token": token, "password": "a long enough password"})
			requireTooMany(t, code, h, body)
		})
	}
}

// The invite pair and the reset pair don't share a bucket any more.
func TestInviteAndResetRoutes_SeparateBuckets(t *testing.T) {
	env := testsupport.NewEnv(t)
	token := strings.Repeat("A", 43)
	for i := 0; i < 10; i++ {
		code, _, _ := postFrom(t, env.URL+"/api/invites/lookup", "", map[string]string{"token": token})
		require.Equal(t, http.StatusNotFound, code)
	}
	code, _, _ := postFrom(t, env.URL+"/api/invites/lookup", "", map[string]string{"token": token})
	require.Equal(t, http.StatusTooManyRequests, code)
	code, _, body := postFrom(t, env.URL+"/api/password-resets/lookup", "", map[string]string{"token": token})
	assert.Equal(t, http.StatusNotFound, code, "the reset routes have their own bucket: %s", body)
}

// AC-005, TC-001 — 10 wrong passwords for one email, then the right one:
// 429 for about 15 minutes. Caleb, on the same device, signs in at once.
func TestLogin_PerEmailLock(t *testing.T) {
	env := testsupport.NewEnvWith(t, testsupport.Options{Limits: api.Limits{Login: roomyClient}})
	for i := 0; i < 10; i++ {
		code, _, body := login(t, env, "", "alice@example.com", "not the password")
		require.Equal(t, http.StatusUnauthorized, code, "try %d: %s", i+1, body)
	}
	code, h, body := login(t, env, "", "alice@example.com", "correct horse battery staple")
	retry := requireTooMany(t, code, h, body)
	assert.InDelta(t, 900, retry, 2, "the lock lasts 15 minutes")
	for _, c := range h.Values("Set-Cookie") {
		assert.NotContains(t, c, "homepad_session=", "a locked email gets no session")
	}

	// The same email in other case is the same lock.
	code, _, _ = login(t, env, "", "Alice@Example.com", "correct horse battery staple")
	assert.Equal(t, http.StatusTooManyRequests, code)

	// Another email is unaffected.
	code, _, body = login(t, env, "", "user@homepad.test", "stitch-user-pw")
	assert.Equal(t, http.StatusOK, code, body)
}

// AC-005 — the lock says nothing about whether the email has an account: an
// unknown email locks the same way.
func TestLogin_PerEmailLock_UnknownEmailToo(t *testing.T) {
	env := testsupport.NewEnvWith(t, testsupport.Options{Limits: api.Limits{Login: roomyClient}})
	for i := 0; i < 10; i++ {
		code, _, _ := login(t, env, "", "ghost@example.com", "not the password")
		require.Equal(t, http.StatusUnauthorized, code)
	}
	code, h, body := login(t, env, "", "ghost@example.com", "not the password")
	requireTooMany(t, code, h, body)
}

// AC-005 — a success before the tenth failure clears that email's count.
func TestLogin_SuccessClearsTheCount(t *testing.T) {
	env := testsupport.NewEnvWith(t, testsupport.Options{Limits: api.Limits{Login: roomyClient}})
	for round := 0; round < 2; round++ {
		for i := 0; i < 9; i++ {
			code, _, _ := login(t, env, "", "alice@example.com", "not the password")
			require.Equal(t, http.StatusUnauthorized, code)
		}
		code, _, body := login(t, env, "", "alice@example.com", "correct horse battery staple")
		require.Equal(t, http.StatusOK, code, "round %d: %s", round+1, body)
	}
}

// AC-006 — the bcrypt routes share one process-wide ceiling, whatever the
// client keys are. A slow refill keeps bcrypt's own time from refilling it
// mid-test; the 120-a-minute default is checked in DefaultLimits.
func TestLimits_ProcessCeilingIsShared(t *testing.T) {
	routes := []struct {
		path string
		body map[string]string
	}{
		{"/api/login", map[string]string{"email": "alice@example.com", "password": "not the password"}},
		{"/api/register", map[string]string{"email": "c@example.com", "password": "a long enough password"}},
		{"/api/invites/accept", map[string]string{"token": strings.Repeat("A", 43), "password": "a long enough password"}},
		{"/api/password-resets/accept", map[string]string{"token": strings.Repeat("A", 43), "password": "a long enough password"}},
	}
	for _, rt := range routes {
		t.Run(rt.path, func(t *testing.T) {
			env := testsupport.NewEnvWith(t, testsupport.Options{
				InviteLimit: roomyLimit,
				Limits: api.Limits{Login: roomyClient, Register: roomyClient, Invite: roomyClient, Reset: roomyClient,
					Ceiling: api.RateLimit{PerMinute: 1, Burst: 2}},
			})
			// Two other bcrypt routes spend the ceiling…
			code, _, _ := login(t, env, "", "user@homepad.test", "not the password")
			require.Equal(t, http.StatusUnauthorized, code)
			code, _, _ = postFrom(t, env.URL+"/api/password-resets/accept", "", map[string]string{"token": strings.Repeat("B", 43), "password": "a long enough password"})
			require.Equal(t, http.StatusNotFound, code)
			// …so this one is refused.
			code, h, body := postFrom(t, env.URL+rt.path, "", rt.body)
			requireTooMany(t, code, h, body)
		})
	}
}

// #109 review — the two lookup routes run no bcrypt (one SHA-256 and one
// indexed read), so they don't spend the ceiling and aren't refused by it.
// Their per-client limit still holds.
func TestLimits_LookupsSkipTheCeiling(t *testing.T) {
	env := testsupport.NewEnvWith(t, testsupport.Options{
		InviteLimit: roomyLimit,
		Limits: api.Limits{Login: roomyClient, Invite: roomyClient, Reset: roomyClient,
			Ceiling: api.RateLimit{PerMinute: 1, Burst: 1}},
	})
	code, _, _ := login(t, env, "", "user@homepad.test", "not the password")
	require.Equal(t, http.StatusUnauthorized, code, "the login spends the only token")
	for _, path := range []string{"/api/invites/lookup", "/api/password-resets/lookup"} {
		for i := 0; i < 3; i++ {
			code, _, body := postFrom(t, env.URL+path, "", map[string]string{"token": strings.Repeat("C", 43)})
			assert.Equal(t, http.StatusNotFound, code, "%s try %d: %s", path, i+1, body)
		}
	}
	code, h, body := login(t, env, "", "user@homepad.test", "not the password")
	requireTooMany(t, code, h, body)
}

// #110 review — the lookups skip the bcrypt ceiling but still have a
// process-wide bound of their own, shared by both lookup routes and every
// client: many clients, each under its own limit, can't send an unbounded
// total. Over it they get the same 429 as every other limit.
func TestLimits_LookupsShareAProcessBound(t *testing.T) {
	tp, err := api.ParseTrustedProxies("127.0.0.1")
	require.NoError(t, err)
	env := testsupport.NewEnvWith(t, testsupport.Options{
		TrustedProxies: tp,
		// One lookup per client before its own bucket is empty: only the
		// process bound can refuse a fresh client.
		Limits: api.Limits{Login: roomyClient, Invite: api.RateLimit{PerMinute: 1, Burst: 1},
			Reset:         api.RateLimit{PerMinute: 1, Burst: 1},
			LookupCeiling: api.RateLimit{PerMinute: 1, Burst: 3}},
	})
	lookup := func(path, client string) (int, http.Header, string) {
		return postFrom(t, env.URL+path, client, map[string]string{"token": strings.Repeat("C", 43)})
	}
	for i, path := range []string{"/api/invites/lookup", "/api/password-resets/lookup", "/api/invites/lookup"} {
		code, _, body := lookup(path, fmt.Sprintf("198.51.100.%d", i+1))
		require.Equal(t, http.StatusNotFound, code, "client %d: %s", i+1, body)
	}
	for i, path := range []string{"/api/invites/lookup", "/api/password-resets/lookup"} {
		code, h, body := lookup(path, fmt.Sprintf("198.51.100.%d", i+10))
		requireTooMany(t, code, h, body)
	}
	// The bcrypt routes have their own ceiling: an empty lookup bound
	// doesn't refuse a sign-in.
	code, _, body := login(t, env, "198.51.100.20", "user@homepad.test", "not the password")
	assert.Equal(t, http.StatusUnauthorized, code, body)
}

// AC-006 — the ceiling doesn't reach the authenticated routes.
func TestLimits_CeilingLeavesOtherRoutesAlone(t *testing.T) {
	env := testsupport.NewEnvWith(t, testsupport.Options{Limits: api.Limits{Ceiling: api.RateLimit{PerMinute: 1, Burst: 1}}})
	code, _, _ := login(t, env, "", "user@homepad.test", "not the password")
	require.Equal(t, http.StatusUnauthorized, code)
	code, _, _ = login(t, env, "", "user@homepad.test", "not the password")
	require.Equal(t, http.StatusTooManyRequests, code)
	assert.Equal(t, http.StatusOK, meStatus(t, env, "admin-session"))
}

// AC-008, TC-003 — behind a trusted proxy, two laptops are two clients: one
// running out doesn't block the other.
func TestLogin_TrustedProxy_TellsClientsApart(t *testing.T) {
	tp, err := api.ParseTrustedProxies("127.0.0.1")
	require.NoError(t, err)
	env := testsupport.NewEnvWith(t, testsupport.Options{TrustedProxies: tp})
	// Short passwords are refused before bcrypt, so no token refills mid-test.
	for i := 0; i < 10; i++ {
		code, _, _ := login(t, env, "198.51.100.1", fmt.Sprintf("a%d@example.com", i), "short")
		require.Equal(t, http.StatusBadRequest, code)
	}
	code, _, _ := login(t, env, "198.51.100.1", "a@example.com", "not the password")
	require.Equal(t, http.StatusTooManyRequests, code, "laptop one is out")
	for i := 0; i < 9; i++ {
		// A forged left-hand entry doesn't move laptop two into laptop one's bucket.
		code, _, body := login(t, env, "198.51.100.1, 198.51.100.2", fmt.Sprintf("b%d@example.com", i), "short")
		require.Equal(t, http.StatusBadRequest, code, "laptop two, try %d: %s", i+1, body)
	}
	code, _, body := login(t, env, "198.51.100.2", "b@example.com", "not the password")
	assert.Equal(t, http.StatusUnauthorized, code, "laptop two's tenth try gets a real answer: %s", body)
}

// AC-008 — set, but the peer is not in it: the header is ignored.
func TestLogin_UntrustedPeer_HeaderIgnored(t *testing.T) {
	tp, err := api.ParseTrustedProxies("10.0.0.0/8")
	require.NoError(t, err)
	env := testsupport.NewEnvWith(t, testsupport.Options{TrustedProxies: tp})
	for i := 0; i < 10; i++ {
		code, _, _ := login(t, env, fmt.Sprintf("198.51.100.%d", i+1), fmt.Sprintf("a%d@example.com", i), "short")
		require.Equal(t, http.StatusBadRequest, code)
	}
	code, _, _ := login(t, env, "198.51.100.200", "a@example.com", "not the password")
	assert.Equal(t, http.StatusTooManyRequests, code)
}

// §3 — the audit lines: the limited client, at most once a minute per key;
// the lock without the email. No email is ever logged.
func TestLimits_LogLines(t *testing.T) {
	buf := captureLog(t)
	// Burst 12 with a slow refill: ten failures, two locked tries, then the
	// client limit.
	env := testsupport.NewEnvWith(t, testsupport.Options{Limits: api.Limits{Login: api.RateLimit{PerMinute: 1, Burst: 12}}})
	for i := 0; i < 12; i++ {
		login(t, env, "", "secret-person@example.com", "not the password")
	}
	login(t, env, "", "secret-person@example.com", "not the password")
	login(t, env, "", "secret-person@example.com", "not the password")
	out := buf.String()
	assert.Equal(t, 1, strings.Count(out, "ratelimit: POST /api/login limited client=127.0.0.1"), out)
	assert.Equal(t, 1, strings.Count(out, "ratelimit: login locked for an email"), out)
	assert.NotContains(t, out, "secret-person")
	assert.NotContains(t, out, "example.com")
}
