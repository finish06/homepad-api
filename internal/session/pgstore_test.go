package session_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/dbguard"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// openTestDB opens + migrates the throwaway test database and empties the
// session-related tables. Skipped when DATABASE_URL is unset; FATAL (never
// skip) on a non-test DSN, before anything destructive (dbguard, #72).
func openTestDB(t *testing.T) (*storage.Store, *pgx.Conn) {
	t.Helper()
	dsn := dbguard.RequireTestDatabase(t)
	ctx := context.Background()

	store, err := storage.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	require.NoError(t, store.Migrate(ctx))

	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close(ctx) })
	_, err = conn.Exec(ctx, `TRUNCATE sessions, users RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	return store, conn
}

func createUser(t *testing.T, store *storage.Store, email, role string) string {
	t.Helper()
	u, err := store.CreateUser(context.Background(), email, "x", role)
	require.NoError(t, err)
	return u.ID
}

func newPgHarness(t *testing.T) harness {
	t.Helper()
	store, _ := openTestDB(t)
	clk := newFakeClock()
	st := session.NewPgStore(store.Pool())
	st.Clock = clk.Now
	return harness{
		store: st,
		clock: clk,
		userA: createUser(t, store, "a@sessions.test", "admin"),
		userB: createUser(t, store, "b@sessions.test", "user"),
	}
}

// AC-011 — only sha256(token) is stored. The raw token appears nowhere in the
// row, in any encoding, and the primary key is exactly its SHA-256.
func TestPgStore_StoresOnlyTheTokenHash(t *testing.T) {
	store, conn := openTestDB(t)
	ctx := context.Background()
	uid := createUser(t, store, "hash@sessions.test", "user")
	st := session.NewPgStore(store.Pool())

	token, created, err := st.Create(ctx, uid, lifetime, "ua")
	require.NoError(t, err)

	var stored []byte
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT token_hash FROM sessions WHERE handle = $1`, created.Handle).Scan(&stored))
	want := sha256.Sum256([]byte(token))
	assert.Equal(t, want[:], stored, "token_hash = sha256(token)")

	raw, err := base64.RawURLEncoding.DecodeString(token)
	require.NoError(t, err)
	needles := []string{token, hex.EncodeToString([]byte(token)), hex.EncodeToString(raw)}

	rows, err := conn.Query(ctx, `SELECT s::text FROM sessions s`)
	require.NoError(t, err)
	defer rows.Close()
	n := 0
	for rows.Next() {
		var row string
		require.NoError(t, rows.Scan(&row))
		n++
		for _, needle := range needles {
			assert.NotContains(t, strings.ToLower(row), strings.ToLower(needle),
				"the sessions row must not contain the raw token in any encoding")
		}
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, 1, n)
}

// The schema refuses anything that is not a 32-byte digest in token_hash, so a
// future code path that tries to store a raw token fails loudly.
func TestPgStore_SchemaRefusesNonDigestTokenHash(t *testing.T) {
	store, conn := openTestDB(t)
	ctx := context.Background()
	uid := createUser(t, store, "check@sessions.test", "user")

	_, err := conn.Exec(ctx,
		`INSERT INTO sessions (token_hash, handle, user_id, expires_at)
		 VALUES (convert_to('a-raw-43-character-token-xxxxxxxxxxxxxxxxxx', 'UTF8'), 'h1', $1, now() + interval '1 day')`, uid)
	assert.Error(t, err, "a non-32-byte token_hash must violate the CHECK")
}

// The 1-minute throttle measured at the row: xmin changes on EVERY update, even
// one that writes identical values, so an unchanged xmin proves no write.
func TestPgStore_PollsInsideTheIntervalDoNotWrite(t *testing.T) {
	store, conn := openTestDB(t)
	ctx := context.Background()
	uid := createUser(t, store, "poll@sessions.test", "user")
	clk := newFakeClock()
	st := session.NewPgStore(store.Pool())
	st.Clock = clk.Now

	token, created, err := st.Create(ctx, uid, lifetime, "")
	require.NoError(t, err)
	xmin := func() string {
		var x string
		require.NoError(t, conn.QueryRow(ctx,
			`SELECT xmin::text FROM sessions WHERE handle = $1`, created.Handle).Scan(&x))
		return x
	}
	before := xmin()

	for i := 0; i < 5; i++ { // a poll every 10s for 50s
		clk.Advance(10 * time.Second)
		s, err := st.Lookup(ctx, token)
		require.NoError(t, err)
		if s.TouchDue {
			t.Fatalf("poll %d: TouchDue inside the interval", i)
		}
		_, err = st.Touch(ctx, token, lifetime) // even a forced Touch must not write
		require.NoError(t, err)
	}
	assert.Equal(t, before, xmin(), "no row write inside the 1-minute interval")

	clk.Advance(10 * time.Second) // 60s since the last write
	slid, err := st.Touch(ctx, token, lifetime)
	require.NoError(t, err)
	require.True(t, slid)
	assert.NotEqual(t, before, xmin(), "the slide after the interval is a real write")
}

// TC-007 at the store level: a second store on a NEW pool (a restarted process)
// resolves the session minted by the first.
func TestPgStore_SessionVisibleFromANewPool(t *testing.T) {
	store, _ := openTestDB(t)
	ctx := context.Background()
	uid := createUser(t, store, "restart@sessions.test", "user")

	token, _, err := session.NewPgStore(store.Pool()).Create(ctx, uid, lifetime, "")
	require.NoError(t, err)

	other, err := storage.Open(ctx, store.DSN)
	require.NoError(t, err)
	defer other.Close()
	got, err := session.NewPgStore(other.Pool()).Lookup(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, uid, got.UserID)
}

// Deleting a user deletes their sessions (FK ON DELETE CASCADE).
func TestPgStore_DeletingUserCascadesSessions(t *testing.T) {
	store, conn := openTestDB(t)
	ctx := context.Background()
	uid := createUser(t, store, "cascade@sessions.test", "user")
	st := session.NewPgStore(store.Pool())
	token, _, err := st.Create(ctx, uid, lifetime, "")
	require.NoError(t, err)

	_, err = conn.Exec(ctx, `DELETE FROM users WHERE id = $1`, uid)
	require.NoError(t, err)
	_, err = st.Lookup(ctx, token)
	assert.True(t, errors.Is(err, session.ErrNotFound))
}

// Migrate runs every migration on every boot. 0017 must be a no-op the second
// time — a sessions table that was dropped or emptied on boot would sign
// everyone out on every deploy, which is the bug this phase fixes.
func TestMigration0017_IdempotentAndKeepsSessions(t *testing.T) {
	store, conn := openTestDB(t)
	ctx := context.Background()
	uid := createUser(t, store, "migrate@sessions.test", "user")
	st := session.NewPgStore(store.Pool())
	token, _, err := st.Create(ctx, uid, lifetime, "")
	require.NoError(t, err)

	require.NoError(t, store.Migrate(ctx))
	require.NoError(t, store.Migrate(ctx))

	_, err = st.Lookup(ctx, token)
	assert.NoError(t, err, "sessions survive a re-run of every migration")

	var idx []string
	rows, err := conn.Query(ctx,
		`SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'sessions'`)
	require.NoError(t, err)
	for rows.Next() {
		var def string
		require.NoError(t, rows.Scan(&def))
		idx = append(idx, def)
	}
	rows.Close()
	joined := strings.Join(idx, "\n")
	assert.Contains(t, joined, "(user_id)", "sessions are indexed on user_id")
	assert.Contains(t, joined, "(expires_at)", "sessions are indexed on expires_at")
	assert.Contains(t, joined, "(token_hash)", "token_hash is the primary key")
	assert.Contains(t, joined, "(handle)", "handle is unique")
}
