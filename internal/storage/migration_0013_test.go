package storage_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// Migration 0013 — the 1–8 tile-count grid_width becomes a 12-column span, with
// the old value preserved and the whole thing safe to re-run (migrations run on
// every boot). Exercised against a real Postgres: seed rows under the OLD
// constraint, run Migrate, check the remap table, run Migrate AGAIN, check the
// spans were not remapped a second time.
func TestMigration0013_RemapsAndIsIdempotent(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test (needs Postgres)")
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, dsn)
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.Migrate(ctx))

	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close(ctx)

	// Put the table back into its PRE-0013 shape so the guarded block runs.
	_, err = conn.Exec(ctx, `TRUNCATE categories, users CASCADE`)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `
		ALTER TABLE categories DROP CONSTRAINT IF EXISTS categories_grid_width_span_check;
		ALTER TABLE categories DROP COLUMN IF EXISTS grid_width_legacy;
		ALTER TABLE categories ALTER COLUMN grid_width SET DEFAULT 3;
		ALTER TABLE categories ADD CONSTRAINT categories_grid_width_check CHECK (grid_width BETWEEN 1 AND 8);`)
	require.NoError(t, err)

	var userID string
	require.NoError(t, conn.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, role) VALUES ('m13@example.com','x','admin') RETURNING id`).Scan(&userID))
	want := map[int]int{1: 3, 2: 4, 3: 6, 4: 6, 5: 12, 6: 12, 7: 12, 8: 12}
	for old := range want {
		_, err := conn.Exec(ctx,
			`INSERT INTO categories (user_id, name, sort_index, grid_width) VALUES ($1, $2, $3, $3)`,
			userID, "w"+string(rune('0'+old)), old)
		require.NoError(t, err)
	}

	// First run: remap.
	require.NoError(t, store.Migrate(ctx))
	got := map[int]int{}
	rows, err := conn.Query(ctx, `SELECT grid_width_legacy, grid_width FROM categories`)
	require.NoError(t, err)
	for rows.Next() {
		var legacy, span int
		require.NoError(t, rows.Scan(&legacy, &span))
		got[legacy] = span
	}
	rows.Close()
	assert.Equal(t, want, got, "remap table from SPEC-app-grid §10.4 (width 4 → 6, Caleb 2026-09-13)")

	// Second run: nothing moves — spans are not remapped as if they were tile counts.
	require.NoError(t, store.Migrate(ctx))
	again := map[int]int{}
	rows, err = conn.Query(ctx, `SELECT grid_width_legacy, grid_width FROM categories`)
	require.NoError(t, err)
	for rows.Next() {
		var legacy, span int
		require.NoError(t, rows.Scan(&legacy, &span))
		again[legacy] = span
	}
	rows.Close()
	assert.Equal(t, want, again, "re-running the migration must be a no-op")

	// New rows default to the half span and the CHECK rejects a tile count.
	var def int
	require.NoError(t, conn.QueryRow(ctx,
		`INSERT INTO categories (user_id, name, sort_index) VALUES ($1, 'fresh', 99) RETURNING grid_width`, userID).Scan(&def))
	assert.Equal(t, 6, def)
	_, err = conn.Exec(ctx, `INSERT INTO categories (user_id, name, sort_index, grid_width) VALUES ($1, 'bad', 100, 5)`, userID)
	assert.Error(t, err, "the span CHECK must reject 5")
}
