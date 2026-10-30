package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/dbguard"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/gatus"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// DEMO-ONLY (demo-seed branch). The public demo (demo.gethomepad.dev) is Cloud
// Run with scale-to-zero, and its Postgres is an ephemeral sidecar: every cold
// start — after idle, and after every deploy — boots against an EMPTY database,
// runs every migration from zero and re-seeds the shared login through
// SeedDemoIfNoUsers. That wipe is the demo's reset-on-idle.
//
// Persistent sessions (#81, migration 0017) put the session store in that same
// database, and kid accounts (#83, 0018) add rows that hang off users. These
// tests boot the demo the way cmd/homepad-api does, against a brand-new
// database standing in for a fresh sidecar, and check that:
//   - the shared demo login works and /api/services serves the curated board;
//   - an API restart on the SAME database keeps visitors signed in and does not
//     re-seed;
//   - a reset (new database) signs every visitor out cleanly — the old cookie
//     is a 401, not an error — re-seeds, and leaves no kid or session behind;
//   - deleting users, however a future reset might do it, takes their sessions
//     with them (the FK cascades), so no session can outlive its user.

const (
	demoEmail    = "homepad@gethomepad.dev"
	demoPassword = "homepad1"
)

// demoBoardSize is the number of tiles in the curated board seed.
func demoBoardSize(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile("../storage/seed/homepad_demo_board.json")
	require.NoError(t, err)
	var board []json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &board))
	require.NotEmpty(t, board)
	return len(board)
}

// freshDemoDB creates an empty throwaway database named name (it must end in
// _test) next to the base DATABASE_URL and returns its DSN: what a demo cold
// start sees. It is a separate database, not a schema, because some migrations
// (0008) probe information_schema without a schema filter. Dropped on cleanup.
func freshDemoDB(t *testing.T, name string) string {
	t.Helper()
	require.True(t, strings.HasSuffix(name, "_test"), "scratch database names must end in _test")
	// Guard the BASE DSN before any DDL against its server (#72).
	base := dbguard.RequireTestDatabase(t)
	ctx := context.Background()
	ident := pgx.Identifier{name}.Sanitize()

	admin, err := pgx.Connect(ctx, base)
	require.NoError(t, err)
	// FORCE clears connections left by an interrupted earlier run.
	_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+ident+` WITH (FORCE)`)
	_, err = admin.Exec(ctx, `CREATE DATABASE `+ident)
	admin.Close(ctx)
	require.NoError(t, err, "create %s", name)
	t.Cleanup(func() {
		a, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Errorf("drop %s: connect: %v", name, err)
			return
		}
		defer a.Close(ctx)
		if _, err := a.Exec(ctx, `DROP DATABASE IF EXISTS `+ident+` WITH (FORCE)`); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})

	u, err := url.Parse(base)
	require.NoError(t, err)
	u.Path = "/" + name
	dsn := u.String()
	require.NoError(t, dbguard.CheckTestDatabase(dsn))
	return dsn
}

// demoInstance is one booted demo API: its store, its server, and whether its
// boot seeded the demo login.
type demoInstance struct {
	store  *storage.Store
	srv    *httptest.Server
	seeded bool
}

// bootDemo boots the API on dsn in cmd/homepad-api's order: Migrate,
// SeedLibraryIfEmpty, SeedDemoIfNoUsers, then a Postgres session store on the
// shared pool.
func bootDemo(t *testing.T, dsn string) demoInstance {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	require.NoError(t, store.Migrate(ctx), "migrate from zero")
	_, err = store.SeedLibraryIfEmpty(ctx)
	require.NoError(t, err)
	seeded, err := store.SeedDemoIfNoUsers(ctx)
	require.NoError(t, err, "SeedDemoIfNoUsers is log.Fatalf at boot: an error here is a demo that never starts")

	// Gatus is a black hole, as in testsupport: every tile reads UNKNOWN.
	poller := gatus.NewPoller(gatus.NewClient("http://127.0.0.1:1"), time.Hour)
	srv := httptest.NewServer(api.New(api.Deps{
		Store:        store,
		Poller:       poller,
		Sessions:     session.NewPgStore(store.Pool()),
		Registration: api.RegistrationOpen,
	}))
	t.Cleanup(srv.Close)
	return demoInstance{store: store, srv: srv, seeded: seeded}
}

func demoLogin(t *testing.T, srv *httptest.Server) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": demoEmail, "password": demoPassword})
	resp, err := http.Post(srv.URL+"/api/login", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "the shared demo login must work")
	ck := sessionCookieFrom(resp)
	require.NotNil(t, ck, "login must set homepad_session")
	return ck
}

// demoDo sends method path (with an optional JSON body) carrying only ck.
func demoDo(t *testing.T, srv *httptest.Server, ck *http.Cookie, method, path string, body any) *http.Response {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(&http.Cookie{Name: ck.Name, Value: ck.Value})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func countRows(t *testing.T, dsn, query string) int {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close(ctx)
	var n int
	require.NoError(t, conn.QueryRow(ctx, query).Scan(&n))
	return n
}

const orphanSessions = `SELECT count(*) FROM sessions s LEFT JOIN users u ON u.id = s.user_id WHERE u.id IS NULL`

func TestDemo_ColdStartLoginServesCuratedBoard(t *testing.T) {
	dsn := freshDemoDB(t, "demo_cold_start_test")
	d := bootDemo(t, dsn)
	require.True(t, d.seeded, "a cold start on an empty database must seed the demo login")

	ck := demoLogin(t, d.srv)

	me := demoDo(t, d.srv, ck, http.MethodGet, "/api/me", nil)
	require.Equal(t, http.StatusOK, me.StatusCode)
	var who struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	require.NoError(t, json.NewDecoder(me.Body).Decode(&who))
	assert.Equal(t, demoEmail, who.Email)
	assert.Equal(t, "admin", who.Role)

	resp := demoDo(t, d.srv, ck, http.MethodGet, "/api/services", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Services []struct {
			Slug         string  `json:"slug"`
			GatusKey     string  `json:"gatus_key"`
			CategoryName *string `json:"categoryName"`
		} `json:"services"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Len(t, out.Services, demoBoardSize(t), "/api/services must serve the whole curated board")
	for _, s := range out.Services {
		assert.Equal(t, "demo_"+s.Slug, s.GatusKey, "tile %s must read its status from the demo Gatus group", s.Slug)
		assert.NotNil(t, s.CategoryName, "tile %s must be in a curated category", s.Slug)
	}

	// The sign-in is a row in the demo's own database, owned by the demo user.
	assert.Equal(t, 1, countRows(t, dsn,
		`SELECT count(*) FROM sessions s JOIN users u ON u.id = s.user_id WHERE u.email = '`+demoEmail+`'`))
}

func TestDemo_APIRestartOnSameDatabaseKeepsVisitorsSignedIn(t *testing.T) {
	dsn := freshDemoDB(t, "demo_restart_test")
	first := bootDemo(t, dsn)
	ck := demoLogin(t, first.srv)

	// The api container restarts but the sidecar (and its data) survives.
	first.srv.Close()
	again := bootDemo(t, dsn)
	assert.False(t, again.seeded, "a reboot with users present must not re-seed")
	assert.Equal(t, 1, countRows(t, dsn, `SELECT count(*) FROM users`))

	assert.Equal(t, http.StatusOK, demoDo(t, again.srv, ck, http.MethodGet, "/api/me", nil).StatusCode,
		"sessions persist in Postgres now: a restart alone must not sign the visitor out")
}

func TestDemo_IdleResetSignsEveryoneOutAndReseeds(t *testing.T) {
	// Before the reset: a visitor signs in, creates a kid account and signs a
	// device in as that kid — all state that lives in the demo database.
	before := freshDemoDB(t, "demo_before_reset_test")
	old := bootDemo(t, before)
	adult := demoLogin(t, old.srv)

	created := demoDo(t, old.srv, adult, http.MethodPost, "/api/admin/kids", map[string]string{"name": "Sam"})
	require.Equal(t, http.StatusCreated, created.StatusCode, "the demo admin can create a kid account")
	var kid struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(created.Body).Decode(&kid))

	other := demoLogin(t, old.srv) // a second visitor on the shared login
	signIn := demoDo(t, old.srv, other, http.MethodPost, "/api/admin/kids/"+kid.ID+"/sign-in-here", nil)
	require.Equal(t, http.StatusNoContent, signIn.StatusCode)
	kidCk := sessionCookieFrom(signIn)
	require.NotNil(t, kidCk)
	require.Equal(t, http.StatusOK, demoDo(t, old.srv, kidCk, http.MethodGet, "/api/me", nil).StatusCode)
	require.Equal(t, 2, countRows(t, before, `SELECT count(*) FROM sessions`))

	// Scale to zero: the instance and its sidecar go away. The next visitor
	// lands on a new instance with an empty database.
	old.srv.Close()
	after := freshDemoDB(t, "demo_after_reset_test")
	fresh := bootDemo(t, after)
	require.True(t, fresh.seeded, "the cold start after a reset must re-seed the demo login")

	for name, ck := range map[string]*http.Cookie{"adult": adult, "kid": kidCk} {
		assert.Equal(t, http.StatusUnauthorized, demoDo(t, fresh.srv, ck, http.MethodGet, "/api/me", nil).StatusCode,
			"a pre-reset %s cookie must read as signed out, not as an error", name)
	}

	// The demo login works again, the board is back, and nothing from before
	// the reset survived: one user, no kids, only the new session.
	ck := demoLogin(t, fresh.srv)
	assert.Equal(t, http.StatusOK, demoDo(t, fresh.srv, ck, http.MethodGet, "/api/services", nil).StatusCode)
	assert.Equal(t, 1, countRows(t, after, `SELECT count(*) FROM users`))
	assert.Equal(t, 0, countRows(t, after, `SELECT count(*) FROM users WHERE role = 'kid'`))
	assert.Equal(t, 0, countRows(t, after, `SELECT count(*) FROM kid_allowed_services`))
	assert.Equal(t, 1, countRows(t, after, `SELECT count(*) FROM sessions`))
	assert.Equal(t, 0, countRows(t, after, orphanSessions))
}

func TestDemo_DeletingUsersTakesTheirSessions(t *testing.T) {
	// Should a reset ever empty the users table in place rather than replace
	// the database, the next boot must still come back clean.
	dsn := freshDemoDB(t, "demo_delete_users_test")
	d := bootDemo(t, dsn)
	adult := demoLogin(t, d.srv)
	created := demoDo(t, d.srv, adult, http.MethodPost, "/api/admin/kids", map[string]string{"name": "Alex"})
	require.Equal(t, http.StatusCreated, created.StatusCode)
	require.Equal(t, 1, countRows(t, dsn, `SELECT count(*) FROM sessions`))

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `DELETE FROM users`)
	conn.Close(ctx)
	require.NoError(t, err)

	assert.Equal(t, 0, countRows(t, dsn, `SELECT count(*) FROM sessions`), "sessions must cascade with their user")
	assert.Equal(t, 0, countRows(t, dsn, orphanSessions))
	assert.Equal(t, http.StatusUnauthorized, demoDo(t, d.srv, adult, http.MethodGet, "/api/me", nil).StatusCode)

	d.srv.Close()
	again := bootDemo(t, dsn)
	require.True(t, again.seeded, "an empty users table must re-seed on the next boot")
	ck := demoLogin(t, again.srv)
	resp := demoDo(t, again.srv, ck, http.MethodGet, "/api/services", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Services []json.RawMessage `json:"services"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Len(t, out.Services, demoBoardSize(t))
	assert.Equal(t, 1, countRows(t, dsn, `SELECT count(*) FROM sessions`))
	assert.Equal(t, 0, countRows(t, dsn, orphanSessions))
}
