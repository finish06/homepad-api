package storage_test

import (
	"context"
	"io/fs"
	"sort"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
	"gitea.kube.calebdunn.tech/code/homepad-api/migrations"
)

// SPEC-household-catalog-owner §6.1 (Q3) — the runner records each migration
// in schema_migrations and runs it once. Before 18.0.0 it re-ran every file on
// every boot, which would bring services.user_id back after 0021 (§1a).

// freshStore is freshDB plus a Store on the same throwaway database.
func freshStore(t *testing.T, name string) (context.Context, *pgx.Conn, *storage.Store) {
	t.Helper()
	ctx, conn := freshDB(t, name)
	store, err := storage.Open(ctx, withDBName(t, requireDatabaseURL(t), name))
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	return ctx, conn, store
}

// embeddedMigrations is every *.up.sql the binary carries, in run order.
func embeddedMigrations(t *testing.T) []string {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.up.sql")
	require.NoError(t, err)
	sort.Strings(names)
	return names
}

// recordedMigrations reads schema_migrations in name order.
func recordedMigrations(t *testing.T, ctx context.Context, conn *pgx.Conn) []string {
	t.Helper()
	rows, err := conn.Query(ctx, `SELECT name FROM schema_migrations ORDER BY name`)
	require.NoError(t, err)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return names
}

func tableExists(t *testing.T, ctx context.Context, conn *pgx.Conn, name string) bool {
	t.Helper()
	var ok bool
	require.NoError(t, conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+name).Scan(&ok))
	return ok
}

// AC-007 — a fresh database runs every file once and records it; a second
// boot runs none.
func TestMigrate_RecordsEveryFileAndRunsItOnce(t *testing.T) {
	ctx, conn, store := freshStore(t, "homepad_runner_fresh_test")
	all := embeddedMigrations(t)
	require.Contains(t, all, "0021_household_catalog.up.sql")

	first, err := store.MigrateReport(ctx)
	require.NoError(t, err)
	assert.Equal(t, all, first.Applied, "the first boot runs every file, in order")
	assert.Equal(t, all, recordedMigrations(t, ctx, conn), "and records every one")

	second, err := store.MigrateReport(ctx)
	require.NoError(t, err)
	assert.Empty(t, second.Applied, "a second boot runs nothing")
	assert.Equal(t, all, recordedMigrations(t, ctx, conn))
}

// AC-007 — a 17.1.0 database has no schema_migrations. The first 18.0.0 boot
// re-runs 0001–0020 once (they are idempotent), runs 0021 and records all of
// them. A second boot runs none.
func TestMigrate_FirstBootOn171DatabaseRunsAllThenNone(t *testing.T) {
	ctx, conn, store := freshStore(t, "homepad_runner_171_test")
	applyMigrations(t, ctx, conn, "0021") // what 17.1.0's runner leaves behind
	require.False(t, tableExists(t, ctx, conn, "schema_migrations"), "17.1.0 kept no version table")

	first, err := store.MigrateReport(ctx)
	require.NoError(t, err)
	assert.Equal(t, embeddedMigrations(t), first.Applied)
	assert.Equal(t, embeddedMigrations(t), recordedMigrations(t, ctx, conn))

	second, err := store.MigrateReport(ctx)
	require.NoError(t, err)
	assert.Empty(t, second.Applied)
}

// Plan risk 1 — a file that fails half way is never recorded: the run is one
// transaction with its records, so a failed boot leaves nothing behind and
// the next boot tries again.
func TestMigrate_AFailedFileRecordsNothing(t *testing.T) {
	ctx, conn, store := freshStore(t, "homepad_runner_fail_test")
	broken := fstest.MapFS{
		"0001_ok.up.sql":     {Data: []byte(`CREATE TABLE runner_ok (id int);`)},
		"0002_broken.up.sql": {Data: []byte(`CREATE TABLE runner_half (id int); SELEC 1;`)},
	}
	_, err := store.MigrateFS(ctx, broken)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0002_broken.up.sql", "the error names the file")

	assert.False(t, tableExists(t, ctx, conn, "runner_ok"), "the earlier file rolled back too")
	assert.False(t, tableExists(t, ctx, conn, "runner_half"), "the failed file left nothing")
	assert.False(t, tableExists(t, ctx, conn, "schema_migrations"), "nothing was recorded")

	// Fixed, the next boot runs both.
	broken["0002_broken.up.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE runner_half (id int);`)}
	rep, err := store.MigrateFS(ctx, broken)
	require.NoError(t, err)
	assert.Equal(t, []string{"0001_ok.up.sql", "0002_broken.up.sql"}, rep.Applied)
	assert.Equal(t, []string{"0001_ok.up.sql", "0002_broken.up.sql"}, recordedMigrations(t, ctx, conn))
}

// §6.1 — a recorded file is skipped, even one that could not run twice.
func TestMigrate_SkipsARecordedFile(t *testing.T) {
	ctx, conn, store := freshStore(t, "homepad_runner_skip_test")
	once := fstest.MapFS{
		"0001_once.up.sql": {Data: []byte(`CREATE TABLE runner_once (id int);`)}, // no IF NOT EXISTS
	}
	_, err := store.MigrateFS(ctx, once)
	require.NoError(t, err)
	rep, err := store.MigrateFS(ctx, once)
	require.NoError(t, err, "a second run would fail if the file ran again")
	assert.Empty(t, rep.Applied)

	// A new file is picked up on its own.
	once["0002_next.up.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE runner_next (id int);`)}
	rep, err = store.MigrateFS(ctx, once)
	require.NoError(t, err)
	assert.Equal(t, []string{"0002_next.up.sql"}, rep.Applied)
	assert.True(t, tableExists(t, ctx, conn, "runner_next"))
}

// rerunMigration forgets name's record and migrates, so that one file runs
// again on the current schema. The tests of migrations written before the
// runner kept records (0013–0020) use it: those files must stay idempotent,
// because the first 18.0.0 boot on a 17.1.0 database re-runs every one of
// them once.
func rerunMigration(t *testing.T, ctx context.Context, store *storage.Store, name string) {
	t.Helper()
	_, err := store.Pool().Exec(ctx, `DELETE FROM schema_migrations WHERE name = $1`, name)
	require.NoError(t, err)
	rep, err := store.MigrateReport(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{name}, rep.Applied, "only the forgotten file runs")
}
