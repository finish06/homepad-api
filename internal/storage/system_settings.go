package storage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// SystemSettings is the global, admin-controlled configuration singleton
// (migration 0010). Today it holds one field — the app-grid uptime-display
// toggle — but the singleton-row shape lets future settings add columns rather
// than tables. A missing row is treated as all-defaults, so a fresh install
// needs no seed data to preserve existing behavior (SPEC cap6 §6, D7).
type SystemSettings struct {
	ShowUptimeDisplay bool
	// StatusDegradedMs is the admin-set "Slow" threshold (migration 0014); nil
	// means "not set in the UI" — the server default / GATUS_DEGRADED_MS applies.
	StatusDegradedMs *int
}

// defaultSystemSettings is the safe-from-absent config: everything ON. Returned
// whenever no system_settings row exists (AC-008).
func defaultSystemSettings() SystemSettings {
	return SystemSettings{ShowUptimeDisplay: true}
}

// SystemSettings reads the singleton settings row (id = 1). When no row exists
// it returns the defaults (ShowUptimeDisplay = true) with a nil error — never
// ErrNotFound — so the caller can always serve a coherent config (AC-008).
func (s *Store) SystemSettings(ctx context.Context) (SystemSettings, error) {
	cfg := defaultSystemSettings()
	err := s.pool.QueryRow(ctx,
		`SELECT show_uptime_display, status_degraded_ms FROM system_settings WHERE id = 1`).
		Scan(&cfg.ShowUptimeDisplay, &cfg.StatusDegradedMs)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultSystemSettings(), nil
	}
	if err != nil {
		return SystemSettings{}, err
	}
	return cfg, nil
}

// UpsertSystemSettings writes the singleton row (id = 1) with the given values
// and returns the persisted state. It is a full upsert of the row's columns; the
// handler layer merges a partial PATCH over the current values before calling
// this, so an omitted field keeps its prior value (SPEC cap6 §7, D5).
func (s *Store) UpsertSystemSettings(ctx context.Context, in SystemSettings) (SystemSettings, error) {
	var out SystemSettings
	err := s.pool.QueryRow(ctx,
		`INSERT INTO system_settings (id, show_uptime_display, status_degraded_ms, updated_at)
		 VALUES (1, $1, $2, now())
		 ON CONFLICT (id) DO UPDATE
		   SET show_uptime_display = EXCLUDED.show_uptime_display,
		       status_degraded_ms = EXCLUDED.status_degraded_ms,
		       updated_at = now()
		 RETURNING show_uptime_display, status_degraded_ms`, in.ShowUptimeDisplay, in.StatusDegradedMs).
		Scan(&out.ShowUptimeDisplay, &out.StatusDegradedMs)
	if err != nil {
		return SystemSettings{}, err
	}
	return out, nil
}
