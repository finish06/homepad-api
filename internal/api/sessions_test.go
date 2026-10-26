package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/dbguard"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// Persistent, hashed sessions through the real HTTP surface
// (SPEC-kid-accounts AC-010, AC-011, TC-002, TC-007; plan TASK-104/105).
// Every server here is wired the way cmd/homepad-api wires production: a
// session.PgStore sharing the storage pool.

const (
	sessEmail    = "sessions@homepad.test"
	sessPassword = "sessions-password"
	adultMaxAge  = 30 * 24 * 60 * 60 // seconds
)

// testClock is a settable clock shared with the session store under test.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// sessionDB opens + migrates the throwaway test DB, empties it and seeds one
// adult user. It returns the DSN (to open further "restarted" instances), the
// store, a raw connection for inspecting rows, and the user's id.
func sessionDB(t *testing.T) (string, *storage.Store, *pgx.Conn, string) {
	t.Helper()
	dsn := dbguard.RequireTestDatabase(t)
	ctx := context.Background()
	store := openStore(t, dsn)
	truncateRegistration(t, ctx, dsn)

	hash, err := bcrypt.GenerateFromPassword([]byte(sessPassword), bcrypt.MinCost)
	require.NoError(t, err)
	u, err := store.CreateUser(ctx, sessEmail, string(hash), "user")
	require.NoError(t, err)

	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close(ctx) })
	return dsn, store, conn, u.ID
}

// openStore opens and migrates a NEW pool on dsn — what a booting process does.
func openStore(t *testing.T, dsn string) *storage.Store {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	require.NoError(t, store.Migrate(ctx))
	return store
}

// startAPI boots an API instance on store with a Postgres session store driven
// by clk (nil = real time).
func startAPI(t *testing.T, store *storage.Store, clk *testClock) *httptest.Server {
	t.Helper()
	st := session.NewPgStore(store.Pool())
	if clk != nil {
		st.Clock = clk.Now
	}
	srv := httptest.NewServer(api.New(api.Deps{
		Store:        store,
		Sessions:     st,
		Registration: api.RegistrationOpen,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sessionCookieFrom(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == "homepad_session" {
			return c
		}
	}
	return nil
}

// loginAs signs in with the seeded credentials; extra cookies (e.g. a previous
// session) are sent along. Returns the new session cookie.
func loginAs(t *testing.T, srv *httptest.Server, userAgent string, send ...*http.Cookie) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": sessEmail, "password": sessPassword})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	for _, c := range send {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "login")
	ck := sessionCookieFrom(resp)
	require.NotNil(t, ck, "login must set homepad_session")
	return ck
}

// meWithCookie calls /api/me with only the given session cookie.
func meWithCookie(t *testing.T, srv *httptest.Server, ck *http.Cookie) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/me", nil)
	req.AddCookie(&http.Cookie{Name: ck.Name, Value: ck.Value})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	return resp
}

type sessionRow struct {
	LastSeen time.Time
	Expires  time.Time
	UA       string
	Xmin     string
}

func rowFor(t *testing.T, conn *pgx.Conn, token string) (sessionRow, bool) {
	t.Helper()
	var r sessionRow
	sum := sha256.Sum256([]byte(token))
	err := conn.QueryRow(context.Background(),
		`SELECT last_seen_at, expires_at, user_agent, xmin::text FROM sessions WHERE token_hash = $1`,
		sum[:]).Scan(&r.LastSeen, &r.Expires, &r.UA, &r.Xmin)
	if err == pgx.ErrNoRows {
		return r, false
	}
	require.NoError(t, err)
	return r, true
}

func countSessions(t *testing.T, conn *pgx.Conn, userID string) int {
	t.Helper()
	var n int
	require.NoError(t, conn.QueryRow(context.Background(),
		`SELECT count(*) FROM sessions WHERE user_id = $1`, userID).Scan(&n))
	return n
}

// TC-007 / AC-010 — an adult signs in, the API restarts (a new process: new
// pool, migrations re-run, new handler), and the same cookie is still signed in.
func TestSessionSurvivesAPIRestart(t *testing.T) {
	dsn, store, _, _ := sessionDB(t)
	first := startAPI(t, store, nil)
	ck := loginAs(t, first, "")
	require.Equal(t, http.StatusOK, meWithCookie(t, first, ck).StatusCode)

	// Kill the first instance completely.
	first.Close()
	store.Close()

	second := startAPI(t, openStore(t, dsn), nil)
	assert.Equal(t, http.StatusOK, meWithCookie(t, second, ck).StatusCode,
		"the same cookie must still be signed in after a restart")
}

// AC-010 — the cookie carries Max-Age matching the adult lifetime and keeps
// HttpOnly + SameSite=Lax.
func TestLoginCookieCarriesMaxAge(t *testing.T) {
	_, store, _, _ := sessionDB(t)
	srv := startAPI(t, store, nil)
	ck := loginAs(t, srv, "")
	assert.Equal(t, adultMaxAge, ck.MaxAge, "Max-Age = 30 days")
	assert.True(t, ck.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, ck.SameSite)
	assert.Equal(t, "/", ck.Path)
}

// AC-011 — scan EVERY table in the schema after two sign-ins: the raw token
// appears nowhere, in any encoding; the sessions row holds sha256(token).
func TestRawSessionTokenNeverInDatabase(t *testing.T) {
	_, store, conn, _ := sessionDB(t)
	srv := startAPI(t, store, nil)
	tokens := []*http.Cookie{loginAs(t, srv, "a"), loginAs(t, srv, "b")}

	ctx := context.Background()
	var needles []string
	for _, ck := range tokens {
		raw, err := base64.RawURLEncoding.DecodeString(ck.Value)
		require.NoError(t, err)
		needles = append(needles,
			strings.ToLower(ck.Value),
			hex.EncodeToString([]byte(ck.Value)),
			hex.EncodeToString(raw))

		var n int
		require.NoError(t, conn.QueryRow(ctx,
			`SELECT count(*) FROM sessions WHERE token_hash = sha256(convert_to($1, 'UTF8'))`,
			ck.Value).Scan(&n))
		assert.Equal(t, 1, n, "the sessions row is keyed by sha256(token)")
	}

	tables, err := conn.Query(ctx,
		`SELECT quote_ident(table_name) FROM information_schema.tables
		 WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`)
	require.NoError(t, err)
	var names []string
	for tables.Next() {
		var n string
		require.NoError(t, tables.Scan(&n))
		names = append(names, n)
	}
	tables.Close()
	require.Contains(t, names, "sessions")

	scanned := 0
	for _, name := range names {
		rows, err := conn.Query(ctx, `SELECT t::text FROM `+name+` t`)
		require.NoError(t, err)
		for rows.Next() {
			var row string
			require.NoError(t, rows.Scan(&row))
			scanned++
			for _, needle := range needles {
				if strings.Contains(strings.ToLower(row), needle) {
					t.Errorf("table %s holds a raw session token: %s", name, row)
				}
			}
		}
		require.NoError(t, rows.Err())
		rows.Close()
	}
	assert.Greater(t, scanned, 2, "the scan actually read rows")
}

// An idle session expires after its lifetime: 401, and no cookie refresh.
func TestSessionExpiresAfterIdleLifetime(t *testing.T) {
	_, store, _, _ := sessionDB(t)
	clk := newTestClock()
	srv := startAPI(t, store, clk)
	ck := loginAs(t, srv, "")

	clk.Advance(30 * 24 * time.Hour)
	resp := meWithCookie(t, srv, ck)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "30 idle days later the session is over")
	assert.Nil(t, sessionCookieFrom(resp), "an expired session is not refreshed")
}

// The slide and its 1-minute write throttle, through /api/me: polls inside the
// minute neither write the row (xmin unchanged) nor re-issue the cookie; the
// first request after it slides expires_at and re-issues the cookie with a
// fresh Max-Age; an active session outlives its original 30 days.
func TestSessionSlidesWithOneMinuteThrottle(t *testing.T) {
	_, store, conn, _ := sessionDB(t)
	clk := newTestClock()
	srv := startAPI(t, store, clk)
	t0 := clk.Now()
	ck := loginAs(t, srv, "")

	r0, ok := rowFor(t, conn, ck.Value)
	require.True(t, ok)
	assert.True(t, r0.LastSeen.Equal(t0))
	assert.True(t, r0.Expires.Equal(t0.Add(30*24*time.Hour)))

	// 30s later: a poll. No write, no cookie.
	clk.Advance(30 * time.Second)
	resp := meWithCookie(t, srv, ck)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Nil(t, sessionCookieFrom(resp), "no cookie re-issue inside the minute")
	r1, _ := rowFor(t, conn, ck.Value)
	assert.Equal(t, r0.Xmin, r1.Xmin, "no row write inside the minute")

	// 60s after the write: the slide.
	clk.Advance(30 * time.Second)
	t1 := clk.Now()
	resp = meWithCookie(t, srv, ck)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	re := sessionCookieFrom(resp)
	require.NotNil(t, re, "the slide re-issues the cookie")
	assert.Equal(t, ck.Value, re.Value, "same token, new expiry")
	assert.Equal(t, adultMaxAge, re.MaxAge)
	assert.True(t, re.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, re.SameSite)
	assert.Equal(t, "/", re.Path)
	r2, _ := rowFor(t, conn, ck.Value)
	assert.True(t, r2.LastSeen.Equal(t1), "last_seen_at slid")
	assert.True(t, r2.Expires.Equal(t1.Add(30*24*time.Hour)), "expires_at slid")

	// 10s later: throttled again.
	clk.Advance(10 * time.Second)
	resp = meWithCookie(t, srv, ck)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Nil(t, sessionCookieFrom(resp))
	r3, _ := rowFor(t, conn, ck.Value)
	assert.Equal(t, r2.Xmin, r3.Xmin)

	// Used every 20 days, it outlives its first 30.
	for day := 20; day <= 60; day += 20 {
		clk.Advance(20 * 24 * time.Hour)
		assert.Equal(t, http.StatusOK, meWithCookie(t, srv, ck).StatusCode, "day %d: still signed in", day)
	}
}

// Logout deletes the row: the old cookie is dead on this instance and on any
// other (a restarted one shares the DB).
func TestLogoutDeletesSessionRow(t *testing.T) {
	dsn, store, conn, uid := sessionDB(t)
	srv := startAPI(t, store, nil)
	ck := loginAs(t, srv, "")
	require.Equal(t, 1, countSessions(t, conn, uid))

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/logout", nil)
	req.AddCookie(&http.Cookie{Name: ck.Name, Value: ck.Value})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	cleared := sessionCookieFrom(resp)
	require.NotNil(t, cleared)
	assert.Equal(t, -1, cleared.MaxAge, "logout clears the cookie")

	assert.Equal(t, 0, countSessions(t, conn, uid), "logout deletes the session row")
	assert.Equal(t, http.StatusUnauthorized, meWithCookie(t, srv, ck).StatusCode)
	other := startAPI(t, openStore(t, dsn), nil)
	assert.Equal(t, http.StatusUnauthorized, meWithCookie(t, other, ck).StatusCode)
}

// The User-Agent is captured on create, and a hostile one never fails sign-in.
func TestLoginCapturesUserAgent(t *testing.T) {
	_, store, conn, _ := sessionDB(t)
	srv := startAPI(t, store, nil)

	ck := loginAs(t, srv, "HomepadTest/1.0 (iPad; CPU OS 18_0)")
	r, ok := rowFor(t, conn, ck.Value)
	require.True(t, ok)
	assert.Equal(t, "HomepadTest/1.0 (iPad; CPU OS 18_0)", r.UA)

	bad := loginAs(t, srv, "Broken/\xff\xfe "+strings.Repeat("x", 4000))
	r, ok = rowFor(t, conn, bad.Value)
	require.True(t, ok)
	assert.LessOrEqual(t, len(r.UA), 512)
}

// Signing in again from a browser that already holds a session replaces that
// session rather than leaving it orphaned for 30 days.
func TestReLoginReplacesThisBrowsersSession(t *testing.T) {
	_, store, conn, uid := sessionDB(t)
	srv := startAPI(t, store, nil)

	first := loginAs(t, srv, "")
	second := loginAs(t, srv, "", first)
	assert.NotEqual(t, first.Value, second.Value)
	assert.Equal(t, 1, countSessions(t, conn, uid), "the browser's old session is replaced")
	assert.Equal(t, http.StatusUnauthorized, meWithCookie(t, srv, first).StatusCode)
	assert.Equal(t, http.StatusOK, meWithCookie(t, srv, second).StatusCode)

	// A different device signing in does not touch the first device's session.
	third := loginAs(t, srv, "")
	assert.Equal(t, 2, countSessions(t, conn, uid))
	assert.Equal(t, http.StatusOK, meWithCookie(t, srv, second).StatusCode)
	assert.Equal(t, http.StatusOK, meWithCookie(t, srv, third).StatusCode)
}
