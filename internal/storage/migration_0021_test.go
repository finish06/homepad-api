package storage_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-household-catalog-owner §6.2 — migration 0021_household_catalog. The
// catalog stops belonging to the first admin: rows nobody could see are
// archived and deleted, services.user_id and categories.user_id are dropped,
// and slugs and section names become unique across the table.
//
// Each test builds a database the way 17.1.0 left it (every file up to 0020,
// run by the old runner, no schema_migrations) in a throwaway database, then
// boots the 18.0.0 runner on it.

const mig0021NoAdmin = "migration 0021: the catalog has apps but no admin account exists. Make one account an admin, then restart."

// household171 is the ids of the seeded 17.1.0 household.
type household171 struct {
	owner, admin2, member, maya, leo string
	// The first admin's rows: these are the catalog.
	media, tools                   string
	jellyfin, sonarr, gitea, uncat string
	// Rows nobody's dashboard showed.
	admin2Media, admin2Jellyfin, admin2Extra string
	memberMine, memberGitea                  string
}

// seedHousehold171 seeds a 17.1.0-shaped install: two admins, a member, two
// kids, favorites, app order, folded sections, two widgets, uploaded icons,
// a Library offer, plus the #87 rows a second admin "created" and the v9-era
// copies a member kept. The leftovers reuse the catalog's slug and section
// name, as real ones do, so the new unique indexes can only build once they
// are gone.
func seedHousehold171(t *testing.T, ctx context.Context, conn *pgx.Conn) household171 {
	t.Helper()
	var h household171
	one := func(dst *string, sql string, args ...any) {
		t.Helper()
		require.NoError(t, conn.QueryRow(ctx, sql, args...).Scan(dst), sql)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := conn.Exec(ctx, sql, args...)
		require.NoError(t, err, sql)
	}
	adult := `INSERT INTO users (email, password_hash, role, show_uptime_display, created_at)
	          VALUES ($1, 'h', $2, TRUE, now() - $3::interval) RETURNING id`
	one(&h.owner, adult, "owner@example.com", "admin", "3 days")
	one(&h.admin2, adult, "admin2@example.com", "admin", "2 days")
	one(&h.member, adult, "member@example.com", "user", "1 day")
	kid := `INSERT INTO users (role, display_name, avatar_color, show_uptime_display) VALUES ('kid', $1, 'blue', TRUE) RETURNING id`
	one(&h.maya, kid, "Maya")
	one(&h.leo, kid, "Leo")

	cat := `INSERT INTO categories (user_id, name, sort_index, grid_width) VALUES ($1, $2, $3, 6) RETURNING id`
	one(&h.media, cat, h.owner, "Media", 0)
	one(&h.tools, cat, h.owner, "Tools", 1)
	one(&h.admin2Media, cat, h.admin2, "Media", 0)
	one(&h.memberMine, cat, h.member, "Mine", 0)

	var offer string
	one(&offer, `INSERT INTO library_apps (name, url, sort_index) VALUES ('Jellyfin', 'https://jellyfin.example.com', 0) RETURNING id`)

	svc := `INSERT INTO services (user_id, slug, name, url, icon, gatus_key, category_id, source_library_id)
	        VALUES ($1, $2, $3, $4, $2, $5, $6, $7) RETURNING id`
	one(&h.jellyfin, svc, h.owner, "jellyfin", "Jellyfin", "https://jellyfin.example.com", "media_jellyfin", h.media, offer)
	one(&h.sonarr, svc, h.owner, "sonarr", "Sonarr", "https://sonarr.example.com", nil, h.media, nil)
	one(&h.gitea, svc, h.owner, "gitea", "Gitea", "https://gitea.example.com", "core_gitea", h.tools, nil)
	one(&h.uncat, svc, h.owner, "notes", "Notes", "https://notes.example.com", nil, nil, nil)
	one(&h.admin2Jellyfin, svc, h.admin2, "jellyfin", "Jellyfin (mine)", "https://jf2.example.com", nil, h.admin2Media, offer)
	one(&h.admin2Extra, svc, h.admin2, "extra", "Extra", "https://extra.example.com", nil, nil, nil)
	one(&h.memberGitea, svc, h.member, "gitea", "My Gitea", "https://gitea.example.com", nil, h.memberMine, nil)

	icon := `INSERT INTO service_icons (service_id, variant, bytes, byte_size, width, height, etag)
	         VALUES ($1, $2, '\x89504e47', 4, 64, 64, $3)`
	exec(icon, h.jellyfin, "light", "e1")
	exec(icon, h.jellyfin, "dark", "e2")
	exec(icon, h.gitea, "light", "e3")
	exec(icon, h.admin2Jellyfin, "light", "e4")

	widget := `INSERT INTO service_widgets (service_id, kind, fields) VALUES ($1, $2, $3)`
	exec(widget, h.jellyfin, "jellyfin", []string{"playing"})
	exec(widget, h.sonarr, "sonarr", []string{"queued", "missing"})

	fav := `INSERT INTO favorites (user_id, service_id) VALUES ($1, $2)`
	exec(fav, h.owner, h.jellyfin)
	exec(fav, h.owner, h.gitea)
	exec(fav, h.admin2, h.jellyfin)
	exec(fav, h.member, h.sonarr)
	exec(fav, h.member, h.memberGitea)
	exec(fav, h.admin2, h.admin2Extra)

	order := `INSERT INTO user_layout (user_id, service_id, sort_index) VALUES ($1, $2, $3)`
	for i, id := range []string{h.gitea, h.jellyfin, h.sonarr} {
		exec(order, h.owner, id, i)
	}
	for i, id := range []string{h.memberGitea, h.sonarr, h.jellyfin, h.uncat} {
		exec(order, h.member, id, i)
	}

	fold := `INSERT INTO user_collapsed_categories (user_id, category_id) VALUES ($1, $2)`
	exec(fold, h.owner, h.media)
	exec(fold, h.member, h.tools)
	exec(fold, h.member, h.memberMine)

	allow := `INSERT INTO kid_allowed_services (user_id, service_id) VALUES ($1, $2)`
	exec(allow, h.maya, h.jellyfin)
	exec(allow, h.maya, h.sonarr)
	exec(allow, h.leo, h.gitea)
	exec(allow, h.leo, h.admin2Extra) // never visible: KidServiceIDs filtered to the owner

	exec(`INSERT INTO system_settings (id, show_uptime_display, clock_enabled) VALUES (1, TRUE, TRUE)`)
	return h
}

// rowsOf returns every row of a query as "a|b|c" strings, sorted.
func rowsOf(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string, args ...any) []string {
	t.Helper()
	rows, err := conn.Query(ctx, sql, args...)
	require.NoError(t, err, sql)
	defer rows.Close()
	var out []string
	for rows.Next() {
		vals, err := rows.Values()
		require.NoError(t, err)
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = toText(v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	require.NoError(t, rows.Err())
	sort.Strings(out)
	return out
}

func toText(v any) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprint(v)
}

// catalogState is every per-person row that points at the catalog, plus the
// catalog's own rows, by id.
type catalogState struct {
	services, categories, icons, widgets          []string
	favorites, layout, collapsed, allowed, offers []string
}

// readCatalogState reads the state; owner limits it to the rows of one owner
// (before 0021) and is "" after it, when there is no owner.
func readCatalogState(t *testing.T, ctx context.Context, conn *pgx.Conn, owner string) catalogState {
	t.Helper()
	svcWhere, catWhere := "", ""
	var args []any
	if owner != "" {
		svcWhere = ` WHERE s.id IN (SELECT id FROM services WHERE user_id = $1)`
		catWhere = ` WHERE c.id IN (SELECT id FROM categories WHERE user_id = $1)`
		args = []any{owner}
	}
	q := func(sql string) []string { return rowsOf(t, ctx, conn, sql, args...) }
	return catalogState{
		services: q(`SELECT s.id::text, s.slug, s.name, s.url, s.icon, COALESCE(s.gatus_key, ''), COALESCE(s.category_id::text, ''),
		                    COALESCE(s.source_library_id::text, ''), s.click_action FROM services s` + svcWhere),
		categories: q(`SELECT c.id::text, c.name, c.sort_index, c.grid_width FROM categories c` + catWhere),
		icons:      q(`SELECT si.service_id::text, si.variant, si.etag FROM service_icons si JOIN services s ON s.id = si.service_id` + svcWhere),
		widgets:    q(`SELECT w.service_id::text, w.kind, array_to_string(w.fields, ',') FROM service_widgets w JOIN services s ON s.id = w.service_id` + svcWhere),
		favorites:  q(`SELECT f.user_id::text, f.service_id::text FROM favorites f JOIN services s ON s.id = f.service_id` + svcWhere),
		layout:     q(`SELECT l.user_id::text, l.service_id::text, l.sort_index FROM user_layout l JOIN services s ON s.id = l.service_id` + svcWhere),
		collapsed:  q(`SELECT u.user_id::text, u.category_id::text FROM user_collapsed_categories u JOIN categories c ON c.id = u.category_id` + catWhere),
		allowed:    q(`SELECT k.user_id::text, k.service_id::text FROM kid_allowed_services k JOIN services s ON s.id = k.service_id` + svcWhere),
		offers:     rowsOf(t, ctx, conn, `SELECT id::text, name, url FROM library_apps`),
	}
}

func columnExists(t *testing.T, ctx context.Context, conn *pgx.Conn, table, col string) bool {
	t.Helper()
	var n int
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`,
		table, col).Scan(&n))
	return n > 0
}

func count(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, conn.QueryRow(ctx, sql, args...).Scan(&n), sql)
	return n
}

// AC-001, AC-002, AC-005, AC-006, AC-008 (TC-001, TC-002, TC-003) — a 17.1.0
// household upgrades with every dashboard as it was, the hidden rows archived,
// and no owner left in the schema.
func TestMigration0021_UpgradesA171Household(t *testing.T) {
	ctx, conn, store := freshStore(t, "homepad_mig0021_household_test")
	applyMigrations(t, ctx, conn, "0021")
	h := seedHousehold171(t, ctx, conn)
	before := readCatalogState(t, ctx, conn, h.owner)
	require.Len(t, before.services, 4)
	require.Len(t, before.favorites, 4, "the seed has favorites on the catalog")

	rep, err := store.MigrateReport(ctx)
	require.NoError(t, err)
	assert.Contains(t, rep.Applied, "0021_household_catalog.up.sql")

	// AC-006 — the first admin's rows survive with the same ids, and every
	// favorite, order row, folded section, allow-list entry, icon and widget
	// that pointed at them still does. Nothing else is left.
	after := readCatalogState(t, ctx, conn, "")
	assert.Equal(t, before, after, "the catalog and everything pointing at it is unchanged")

	// AC-005 — the hidden rows are in catalog_archive with their former owner,
	// and gone from the catalog.
	assert.ElementsMatch(t, []string{
		"category|" + h.admin2 + "|admin2@example.com|" + h.admin2Media + "|Media",
		"category|" + h.member + "|member@example.com|" + h.memberMine + "|Mine",
		"service|" + h.admin2 + "|admin2@example.com|" + h.admin2Extra + "|extra",
		"service|" + h.admin2 + "|admin2@example.com|" + h.admin2Jellyfin + "|jellyfin",
		"service|" + h.member + "|member@example.com|" + h.memberGitea + "|gitea",
	}, rowsOf(t, ctx, conn, `SELECT kind, former_owner_id::text, former_owner_email, row->>'id',
	                               COALESCE(row->>'slug', row->>'name') FROM catalog_archive`))
	assert.Equal(t, "https://jf2.example.com", rowsOf(t, ctx, conn,
		`SELECT row->>'url' FROM catalog_archive WHERE row->>'id' = $1`, h.admin2Jellyfin)[0], "the snapshot is the whole row")
	assert.Equal(t, 3, rep.ArchivedServices)
	assert.Equal(t, 2, rep.ArchivedSections)
	assert.Equal(t, "catalog: archived 3 services and 2 sections that no dashboard showed (catalog_archive)", rep.ArchiveLine())
	// Their icons, favorites, order, folds and allow-list rows went with them.
	assert.Equal(t, 3, count(t, ctx, conn, `SELECT count(*) FROM service_icons`))
	assert.Equal(t, 0, count(t, ctx, conn, `SELECT count(*) FROM kid_allowed_services WHERE service_id::text = $1`, h.admin2Extra))
	assert.Equal(t, 0, count(t, ctx, conn, `SELECT count(*) FROM user_collapsed_categories WHERE category_id::text = $1`, h.memberMine))
	// Accounts and settings are untouched.
	assert.Equal(t, 5, count(t, ctx, conn, `SELECT count(*) FROM users`))
	assert.Equal(t, 1, count(t, ctx, conn, `SELECT count(*) FROM system_settings WHERE clock_enabled`))

	// AC-001 — no owner column, and no foreign key from the catalog to users.
	assert.False(t, columnExists(t, ctx, conn, "services", "user_id"))
	assert.False(t, columnExists(t, ctx, conn, "categories", "user_id"))
	assert.Zero(t, count(t, ctx, conn, `SELECT count(*) FROM pg_constraint
	    WHERE contype = 'f' AND conrelid IN ('services'::regclass, 'categories'::regclass) AND confrelid = 'users'::regclass`))

	// AC-002 — slugs and section names are unique across the table.
	assert.True(t, indexExists(t, ctx, conn, "services_slug_unique"))
	assert.True(t, indexExists(t, ctx, conn, "categories_name_unique"))
	for _, gone := range []string{"services_user_slug_key", "services_by_user_idx", "categories_user_name_key", "categories_by_user_idx"} {
		assert.False(t, indexExists(t, ctx, conn, gone), gone)
	}
	assertUniqueViolation(t, ctx, conn, `INSERT INTO services (slug, name, url) VALUES ('jellyfin', 'Again', 'https://x.example.com')`)
	assertUniqueViolation(t, ctx, conn, `INSERT INTO categories (name, sort_index) VALUES ('Media', 9)`)

	// AC-008, TC-003 — two more boots run nothing and leave user_id dropped.
	for range 2 {
		again, err := store.MigrateReport(ctx)
		require.NoError(t, err)
		assert.Empty(t, again.Applied)
		assert.Empty(t, again.ArchiveLine(), "no new log line from 0021")
		assert.False(t, columnExists(t, ctx, conn, "services", "user_id"), "the 0007 re-run trap")
		assert.False(t, columnExists(t, ctx, conn, "categories", "user_id"))
	}
	assert.Equal(t, 5, count(t, ctx, conn, `SELECT count(*) FROM catalog_archive`), "nothing archived twice")

	// AC-001, TC-002 — deleting the first admin (by hand) deletes no app,
	// section, icon or widget.
	_, err = conn.Exec(ctx, `DELETE FROM users WHERE id = $1`, h.owner)
	require.NoError(t, err)
	left := readCatalogState(t, ctx, conn, "")
	assert.Equal(t, before.services, left.services)
	assert.Equal(t, before.categories, left.categories)
	assert.Equal(t, before.icons, left.icons)
	assert.Equal(t, before.widgets, left.widgets)
}

func assertUniqueViolation(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string) {
	t.Helper()
	_, err := conn.Exec(ctx, sql)
	var pgErr *pgconn.PgError
	if assert.True(t, errors.As(err, &pgErr), "%s: want a unique violation, got %v", sql, err) {
		assert.Equal(t, "23505", pgErr.Code, sql)
	}
}

// AC-009, TC-006 — catalog rows and no admin: the boot stops with a plain
// message, and nothing is changed.
func TestMigration0021_CatalogWithNoAdminStopsTheBoot(t *testing.T) {
	ctx, conn, store := freshStore(t, "homepad_mig0021_noadmin_test")
	applyMigrations(t, ctx, conn, "0021")
	h := seedHousehold171(t, ctx, conn)
	_, err := conn.Exec(ctx, `UPDATE users SET role = 'user' WHERE role = 'admin'`)
	require.NoError(t, err)
	before := readCatalogState(t, ctx, conn, h.owner)
	allBefore := count(t, ctx, conn, `SELECT count(*) FROM services`)

	_, err = store.MigrateReport(ctx)
	require.Error(t, err)
	assert.Equal(t, mig0021NoAdmin, err.Error(), "the boot error is the message itself")

	assert.True(t, columnExists(t, ctx, conn, "services", "user_id"), "nothing is changed")
	assert.True(t, columnExists(t, ctx, conn, "categories", "user_id"))
	assert.False(t, tableExists(t, ctx, conn, "catalog_archive"))
	assert.False(t, tableExists(t, ctx, conn, "schema_migrations"), "nothing is recorded")
	assert.Equal(t, allBefore, count(t, ctx, conn, `SELECT count(*) FROM services`))
	assert.Equal(t, before, readCatalogState(t, ctx, conn, h.owner))

	// Make one account an admin, then restart: the upgrade goes through.
	_, err = conn.Exec(ctx, `UPDATE users SET role = 'admin' WHERE id = $1`, h.owner)
	require.NoError(t, err)
	_, err = store.MigrateReport(ctx)
	require.NoError(t, err)
	assert.False(t, columnExists(t, ctx, conn, "services", "user_id"))
}

// AC-009 — the guard is about catalog rows: accounts with no admin and an
// empty catalog migrate.
func TestMigration0021_NoAdminAndNoCatalogMigrates(t *testing.T) {
	ctx, conn, store := freshStore(t, "homepad_mig0021_noadmin_empty_test")
	applyMigrations(t, ctx, conn, "0021")
	_, err := conn.Exec(ctx, `INSERT INTO users (email, password_hash, role, show_uptime_display) VALUES ('m@example.com', 'h', 'user', TRUE)`)
	require.NoError(t, err)

	rep, err := store.MigrateReport(ctx)
	require.NoError(t, err)
	assert.Empty(t, rep.ArchiveLine(), "nothing archived, nothing logged")
	assert.False(t, columnExists(t, ctx, conn, "services", "user_id"))
}

// schemaOf describes every column, index and constraint in the public schema.
func schemaOf(t *testing.T, ctx context.Context, conn *pgx.Conn) []string {
	t.Helper()
	var out []string
	for _, sql := range []string{
		`SELECT 'column', table_name, column_name, data_type, is_nullable, COALESCE(column_default, '')
		   FROM information_schema.columns WHERE table_schema = 'public'`,
		`SELECT 'index', tablename, indexname, indexdef FROM pg_indexes WHERE schemaname = 'public'`,
		`SELECT 'constraint', cl.relname, co.conname, pg_get_constraintdef(co.oid)
		   FROM pg_constraint co JOIN pg_class cl ON cl.oid = co.conrelid
		   JOIN pg_namespace n ON n.oid = cl.relnamespace WHERE n.nspname = 'public'`,
		`SELECT 'extension', extname FROM pg_extension`,
	} {
		out = append(out, rowsOf(t, ctx, conn, sql)...)
	}
	sort.Strings(out)
	return out
}

// AC-010, TC-005 — a fresh, empty database ends in the same schema as an
// upgraded 17.1.0 one.
func TestMigration0021_FreshSchemaMatchesUpgradedSchema(t *testing.T) {
	ctx, upgraded, upStore := freshStore(t, "homepad_mig0021_up_test")
	applyMigrations(t, ctx, upgraded, "0021")
	seedHousehold171(t, ctx, upgraded)
	_, err := upStore.MigrateReport(ctx)
	require.NoError(t, err)

	_, fresh, freshStore := freshStore(t, "homepad_mig0021_fresh_test")
	_, err = freshStore.MigrateReport(ctx)
	require.NoError(t, err)

	want := schemaOf(t, ctx, fresh)
	got := schemaOf(t, ctx, upgraded)
	require.NotEmpty(t, want)
	assert.Equal(t, want, got, "upgraded and fresh schemas differ")
	assert.Equal(t, recordedMigrations(t, ctx, fresh), recordedMigrations(t, ctx, upgraded))
}

// §6.3 — there is no working down migration: the file is only a comment that
// points at the database backup.
func TestMigration0021_DownIsOnlyAComment(t *testing.T) {
	b, err := os.ReadFile("../../migrations/0021_household_catalog.down.sql")
	require.NoError(t, err)
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		assert.True(t, line == "" || strings.HasPrefix(line, "--"), "only comments: %q", line)
	}
	assert.Contains(t, string(b), "CHANGELOG")
}
