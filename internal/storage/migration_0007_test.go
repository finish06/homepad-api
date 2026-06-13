package storage_test

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/migrations"
)

// Migration 0007 (v9.1) is the global→per-user cutover, and is effectively
// one-way. These tests exercise it in isolation on a throwaway database so we
// can stage a realistic *pre*-cutover (v5-style, global catalog) state, run
// 0007, and assert the transform — the part Joe flagged as highest-risk.
//
// ACs: A1 (safe on empty), A2 (seeds library_apps from the existing N services
// + reassigns to the first admin), A3-storage (first admin owns the whole
// surviving catalog; a second user owns nothing).

// withDBName rewrites the database path of a libpq URL DSN.
func withDBName(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err, "parse DATABASE_URL")
	u.Path = "/" + name
	return u.String()
}

// freshDB creates a throwaway database off the base DATABASE_URL and returns a
// connection to it; the database is dropped on cleanup. Skips when
// DATABASE_URL is unset (mirrors the other integration tests).
func freshDB(t *testing.T, name string) (context.Context, *pgx.Conn) {
	t.Helper()
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		t.Skip("DATABASE_URL not set — skipping migration test (needs Postgres)")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, base)
	require.NoError(t, err, "connect to base DB")
	// FORCE drops any lingering connections from a previous interrupted run.
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err, "create temp DB %s", name)
	require.NoError(t, admin.Close(ctx))

	conn, err := pgx.Connect(ctx, withDBName(t, base, name))
	require.NoError(t, err, "connect to temp DB %s", name)
	t.Cleanup(func() {
		conn.Close(ctx)
		a, err := pgx.Connect(ctx, base)
		if err != nil {
			return
		}
		defer a.Close(ctx)
		_, _ = a.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return ctx, conn
}

// applyMigrations execs every embedded *.up.sql whose filename sorts strictly
// before stopBefore (e.g. "0007" applies 0001..0006). stopBefore == "" applies
// all of them.
func applyMigrations(t *testing.T, ctx context.Context, conn *pgx.Conn, stopBefore string) {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.up.sql")
	require.NoError(t, err)
	sort.Strings(names)
	for _, name := range names {
		if stopBefore != "" && name >= stopBefore {
			continue
		}
		sqlBytes, err := migrations.FS.ReadFile(name)
		require.NoError(t, err)
		_, err = conn.Exec(ctx, string(sqlBytes))
		require.NoError(t, err, "apply %s", name)
	}
}

// applyMigrationFile execs a single embedded migration by filename.
func applyMigrationFile(t *testing.T, ctx context.Context, conn *pgx.Conn, name string) {
	t.Helper()
	sqlBytes, err := migrations.FS.ReadFile(name)
	require.NoError(t, err, "0007 migration must exist (RED until written)")
	_, err = conn.Exec(ctx, string(sqlBytes))
	require.NoError(t, err, "apply %s", name)
}

const mig0007 = "0007_per_user_dashboards.up.sql"

// A2 + A3-storage: a populated v5 catalog becomes the first admin's personal
// dashboard and seeds the library.
func TestMigration0007_PopulatedCutover(t *testing.T) {
	ctx, conn := freshDB(t, "homepad_mig0007_pop")
	applyMigrations(t, ctx, conn, "0007")

	// Two users; the admin is created FIRST so it is the "first admin"
	// (role='admin' ORDER BY created_at) the transform must pick.
	var adminID, userID string
	require.NoError(t, conn.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, role, created_at)
		 VALUES ('finish.06@gmail.com','x','admin', now() - interval '1 hour') RETURNING id`).Scan(&adminID))
	require.NoError(t, conn.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, role) VALUES ('gracie@example.com','x','user') RETURNING id`).Scan(&userID))

	// Two categories + 3 services (two categorized, one not) — the pre-cutover
	// global catalog.
	var mediaID, toolsID string
	require.NoError(t, conn.QueryRow(ctx, `INSERT INTO categories (name, sort_index) VALUES ('Media', 0) RETURNING id`).Scan(&mediaID))
	require.NoError(t, conn.QueryRow(ctx, `INSERT INTO categories (name, sort_index) VALUES ('Tools', 1) RETURNING id`).Scan(&toolsID))
	_, err := conn.Exec(ctx, `INSERT INTO services (slug, name, url, icon, category_id) VALUES
		('jellyfin','Jellyfin','https://jf.example.com','jellyfin',$1),
		('gitea','Gitea','https://gitea.example.com','gitea',$2),
		('uncat','Uncat','https://u.example.com','uncat',NULL)`, mediaID, toolsID)
	require.NoError(t, err)

	applyMigrationFile(t, ctx, conn, mig0007)

	// A2 — library seeded with one offer per existing service.
	var libN int
	require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM library_apps`).Scan(&libN))
	assert.Equal(t, 3, libN, "library_apps must have one offer per existing service")

	// suggested_category carries the v4 category name (or '' for uncategorized).
	var jfSuggested string
	require.NoError(t, conn.QueryRow(ctx, `SELECT suggested_category FROM library_apps WHERE name='Jellyfin'`).Scan(&jfSuggested))
	assert.Equal(t, "Media", jfSuggested, "offer suggested_category = its v4 category name")

	// A3-storage — every service + category is reassigned to the first admin.
	var svcOwned, catOwned int
	require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM services WHERE user_id=$1`, adminID).Scan(&svcOwned))
	require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM categories WHERE user_id=$1`, adminID).Scan(&catOwned))
	assert.Equal(t, 3, svcOwned, "all services reassigned to the first admin")
	assert.Equal(t, 2, catOwned, "all categories reassigned to the first admin")

	// The second user owns nothing — an empty dashboard.
	var userSvc int
	require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM services WHERE user_id=$1`, userID).Scan(&userSvc))
	assert.Equal(t, 0, userSvc, "a non-first-admin user starts with an empty dashboard")

	// 5b provenance — each reassigned copy links back to its minted offer.
	var unwired int
	require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM services WHERE source_library_id IS NULL`).Scan(&unwired))
	assert.Equal(t, 0, unwired, "every migrated service has source_library_id wired (5b)")

	// Uniqueness swapped global → per-user.
	assert.False(t, indexExists(t, ctx, conn, "services_slug_key"), "global services_slug_key must be dropped")
	assert.True(t, indexExists(t, ctx, conn, "services_user_slug_key"), "per-user services_user_slug_key must exist")
	assert.False(t, indexExists(t, ctx, conn, "categories_name_key"), "global categories_name_key must be dropped")
	assert.True(t, indexExists(t, ctx, conn, "categories_user_name_key"), "per-user categories_user_name_key must exist")

	// user_id is NOT NULL after backfill.
	assert.True(t, columnNotNull(t, ctx, conn, "services", "user_id"), "services.user_id must be NOT NULL")
	assert.True(t, columnNotNull(t, ctx, conn, "categories", "user_id"), "categories.user_id must be NOT NULL")

	// Two users can now hold the same slug (per-user uniqueness) — sanity.
	_, err = conn.Exec(ctx, `INSERT INTO services (slug, name, url, user_id) VALUES ('jellyfin','J2','https://x',$1)`, userID)
	assert.NoError(t, err, "a second user may reuse a slug another user owns")
}

// A1: 0007 is safe on an EMPTY catalog — library empty, NOT NULL added
// vacuously, indexes present, global uniques gone, no error.
func TestMigration0007_EmptyCatalog(t *testing.T) {
	ctx, conn := freshDB(t, "homepad_mig0007_empty")
	applyMigrations(t, ctx, conn, "") // ALL migrations including 0007, on an empty DB

	var libN int
	require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM library_apps`).Scan(&libN))
	assert.Equal(t, 0, libN, "empty catalog → empty library")

	assert.False(t, indexExists(t, ctx, conn, "services_slug_key"))
	assert.True(t, indexExists(t, ctx, conn, "services_user_slug_key"))
	assert.True(t, columnNotNull(t, ctx, conn, "services", "user_id"))
	assert.True(t, columnNotNull(t, ctx, conn, "categories", "user_id"))
}

func indexExists(t *testing.T, ctx context.Context, conn *pgx.Conn, name string) bool {
	t.Helper()
	// pg_indexes covers both CREATE UNIQUE INDEX and unique CONSTRAINT-backed
	// indexes (a UNIQUE constraint creates an index of the same name).
	var n int
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname=$1`, name).Scan(&n))
	if n > 0 {
		return true
	}
	// Constraints not always surfaced in pg_indexes across versions — check
	// pg_constraint too.
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint WHERE conname=$1`, name).Scan(&n))
	return n > 0
}

func columnNotNull(t *testing.T, ctx context.Context, conn *pgx.Conn, table, col string) bool {
	t.Helper()
	var notnull bool
	err := conn.QueryRow(ctx,
		`SELECT attnotnull FROM pg_attribute
		  WHERE attrelid = $1::regclass AND attname = $2 AND NOT attisdropped`,
		table, col).Scan(&notnull)
	require.NoError(t, err)
	return notnull
}

// guard against an accidental unused-import if helpers change.
var _ = strings.TrimSpace
var _ = fmt.Sprintf
