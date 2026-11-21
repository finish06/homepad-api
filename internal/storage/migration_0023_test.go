package storage_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-iframe-sandbox-optin §6 — migration 0023_iframe_same_origin. It adds
// services.iframe_same_origin (BOOLEAN NOT NULL DEFAULT FALSE) and, in the
// same batch, turns it on for every app already on Inline overlay (Q1,
// decided: "keep them opt in"). It runs once under the runner.

const mig0023 = "0023_iframe_same_origin.up.sql"

// AC-007 — the column exists with the right type, nullability and default.
func TestMigration0023_AddsTheColumn(t *testing.T) {
	ctx, conn, store := freshStore(t, "homepad_mig0023_column_test")
	_, err := store.MigrateReport(ctx)
	require.NoError(t, err)
	assert.Contains(t, embeddedMigrations(t), mig0023)

	var dataType, nullable, def string
	require.NoError(t, conn.QueryRow(ctx, `SELECT data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'services' AND column_name = 'iframe_same_origin'`).
		Scan(&dataType, &nullable, &def))
	assert.Equal(t, "boolean", dataType)
	assert.Equal(t, "NO", nullable)
	assert.Equal(t, "false", def)
}

// AC-008, TC-001 — a 17.1.0 household upgrades: its Inline overlay apps are
// opted in, every other app is not, and apps added or switched to Inline
// overlay afterwards start at false. A second boot changes nothing.
func TestMigration0023_OptsInTheOverlayAppsOnce(t *testing.T) {
	ctx, conn, store := freshStore(t, "homepad_mig0023_upgrade_test")
	applyMigrations(t, ctx, conn, "0021") // what 17.1.0's runner leaves behind
	h := seedHousehold171(t, ctx, conn)
	_, err := conn.Exec(ctx, `UPDATE services SET click_action = 'iframe' WHERE id IN ($1, $2)`, h.jellyfin, h.gitea)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `UPDATE services SET click_action = 'same_tab' WHERE id = $1`, h.sonarr)
	require.NoError(t, err)

	rep, err := store.MigrateReport(ctx)
	require.NoError(t, err)
	assert.Contains(t, rep.Applied, mig0023)

	assert.ElementsMatch(t, []string{
		"gitea|iframe|true",
		"jellyfin|iframe|true",
		"notes|new_tab|false",
		"sonarr|same_tab|false",
	}, rowsOf(t, ctx, conn, `SELECT slug, click_action, iframe_same_origin FROM services`))

	// Apps added later start at false, on Inline overlay or not.
	_, err = conn.Exec(ctx, `INSERT INTO services (slug, name, url, click_action) VALUES ('late', 'Late', 'https://late.example.com', 'iframe')`)
	require.NoError(t, err)
	// An app switched to Inline overlay later keeps false.
	_, err = conn.Exec(ctx, `UPDATE services SET click_action = 'iframe' WHERE id = $1`, h.uncat)
	require.NoError(t, err)
	// An admin turns one upgraded app off.
	_, err = conn.Exec(ctx, `UPDATE services SET iframe_same_origin = FALSE WHERE id = $1`, h.gitea)
	require.NoError(t, err)

	// Boots after the upgrade run nothing, so nothing is turned back on.
	again, err := store.MigrateReport(ctx)
	require.NoError(t, err)
	assert.Empty(t, again.Applied)
	assert.ElementsMatch(t, []string{
		"gitea|iframe|false",
		"jellyfin|iframe|true",
		"late|iframe|false",
		"notes|iframe|false",
		"sonarr|same_tab|false",
	}, rowsOf(t, ctx, conn, `SELECT slug, click_action, iframe_same_origin FROM services`))
}

// §6 — the backfill runs only in the batch that adds the column. Run again on
// a database that already has it (a forgotten record), the file changes
// nothing.
func TestMigration0023_ReRunDoesNotBackfillAgain(t *testing.T) {
	ctx, conn, store := freshStore(t, "homepad_mig0023_rerun_test")
	_, err := store.MigrateReport(ctx)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `INSERT INTO services (slug, name, url, click_action) VALUES ('ov', 'Overlay', 'https://ov.example.com', 'iframe')`)
	require.NoError(t, err)

	rerunMigration(t, ctx, store, mig0023)
	assert.Equal(t, []string{"ov|false"}, rowsOf(t, ctx, conn, `SELECT slug, iframe_same_origin FROM services`))
}

// §6 — the down migration drops the column.
func TestMigration0023_DownDropsTheColumn(t *testing.T) {
	ctx, conn, store := freshStore(t, "homepad_mig0023_down_test")
	_, err := store.MigrateReport(ctx)
	require.NoError(t, err)
	down, err := os.ReadFile("../../migrations/0023_iframe_same_origin.down.sql")
	require.NoError(t, err)
	_, err = conn.Exec(ctx, string(down))
	require.NoError(t, err)
	assert.False(t, columnExists(t, ctx, conn, "services", "iframe_same_origin"))
	_, err = conn.Exec(ctx, string(down))
	require.NoError(t, err, "IF EXISTS: a second run is harmless")
}
