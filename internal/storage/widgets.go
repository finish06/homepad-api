package storage

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Status widgets (SPEC-status-widgets §9, migration 0020).
//
// A widget belongs to its service row, which belongs to the shared-catalog
// owner, so a widget is shared catalog data (SPEC-245-224). The key is stored
// only as AES-256-GCM ciphertext plus its nonce; this package never sees the
// plaintext.

// Widget is one service's widget config.
type Widget struct {
	ServiceID        string
	Kind             string
	Address          string // "" = use the service URL
	Username         string
	SecretCiphertext []byte // nil = no key
	SecretNonce      []byte
	Fields           []string
	IntervalS        int
	InsecureTLS      bool
	Enabled          bool
	// ServiceURL is the tile's link, joined in on read (the address a blank
	// widget address means). Ignored on write.
	ServiceURL string
	UpdatedAt  time.Time
}

const widgetColumns = `w.service_id, w.kind, w.address, w.username, w.secret_ciphertext, w.secret_nonce,
	w.fields, w.interval_s, w.insecure_tls, w.enabled, s.url, w.updated_at`

func scanWidget(row pgx.Row) (Widget, error) {
	var w Widget
	err := row.Scan(&w.ServiceID, &w.Kind, &w.Address, &w.Username, &w.SecretCiphertext, &w.SecretNonce,
		&w.Fields, &w.IntervalS, &w.InsecureTLS, &w.Enabled, &w.ServiceURL, &w.UpdatedAt)
	if w.Fields == nil {
		w.Fields = []string{}
	}
	return w, err
}

func isNoRowsOrBadID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pgErr) && pgErr.Code == "22P02")
}

// Widget returns a service's widget, or ErrNotFound.
func (s *Store) Widget(ctx context.Context, serviceID string) (Widget, error) {
	w, err := scanWidget(s.pool.QueryRow(ctx,
		`SELECT `+widgetColumns+` FROM service_widgets w JOIN services s ON s.id = w.service_id
		  WHERE w.service_id = $1`, serviceID))
	if isNoRowsOrBadID(err) {
		return Widget{}, ErrNotFound
	}
	return w, err
}

// PutWidget creates or whole-replaces a service's widget and returns it.
// ErrNotFound when the service doesn't exist.
func (s *Store) PutWidget(ctx context.Context, in Widget) (Widget, error) {
	fields := in.Fields
	if fields == nil {
		fields = []string{}
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO service_widgets
		   (service_id, kind, address, username, secret_ciphertext, secret_nonce, fields, interval_s, insecure_tls, enabled)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 ON CONFLICT (service_id) DO UPDATE SET
		   kind = EXCLUDED.kind, address = EXCLUDED.address, username = EXCLUDED.username,
		   secret_ciphertext = EXCLUDED.secret_ciphertext, secret_nonce = EXCLUDED.secret_nonce,
		   fields = EXCLUDED.fields, interval_s = EXCLUDED.interval_s,
		   insecure_tls = EXCLUDED.insecure_tls, enabled = EXCLUDED.enabled, updated_at = now()`,
		in.ServiceID, in.Kind, in.Address, in.Username, in.SecretCiphertext, in.SecretNonce,
		fields, in.IntervalS, in.InsecureTLS, in.Enabled)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "22P02") { // no such service / malformed id
		return Widget{}, ErrNotFound
	}
	if err != nil {
		return Widget{}, err
	}
	return s.Widget(ctx, in.ServiceID)
}

// DeleteWidget removes a service's widget. Deleting a missing one is not an
// error.
func (s *Store) DeleteWidget(ctx context.Context, serviceID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM service_widgets WHERE service_id = $1`, serviceID)
	if isNoRowsOrBadID(err) {
		return nil
	}
	return err
}

// EnabledWidgets lists every enabled widget, for the widget manager.
func (s *Store) EnabledWidgets(ctx context.Context) ([]Widget, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+widgetColumns+` FROM service_widgets w JOIN services s ON s.id = w.service_id
		  WHERE w.enabled ORDER BY w.service_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Widget
	for rows.Next() {
		w, err := scanWidget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// WidgetSettings is the clock, weather and web search config on the
// system_settings singleton (migration 0020). It is read and written apart
// from the cap6 settings, so neither write clobbers the other.
type WidgetSettings struct {
	ClockEnabled     bool
	ClockHour12      *bool // nil = the device's choice
	WeatherEnabled   bool
	WeatherPlace     string
	WeatherLat       *float64 // stored rounded to 2 decimals
	WeatherLon       *float64
	WeatherUnits     string // "c" or "f"
	WebSearchEnabled bool
	WebSearchURL     string // https, contains {query}
	KidWebSearch     bool
}

// DefaultWebSearchURL is DuckDuckGo, the default provider (Q8).
const DefaultWebSearchURL = "https://duckduckgo.com/?q={query}"

func defaultWidgetSettings() WidgetSettings {
	return WidgetSettings{WeatherUnits: "c", WebSearchURL: DefaultWebSearchURL}
}

const widgetSettingsColumns = `clock_enabled, clock_hour12, weather_enabled, weather_place,
	weather_lat::float8, weather_lon::float8, weather_units, web_search_enabled, web_search_url, kid_web_search`

func scanWidgetSettings(row pgx.Row) (WidgetSettings, error) {
	var ws WidgetSettings
	err := row.Scan(&ws.ClockEnabled, &ws.ClockHour12, &ws.WeatherEnabled, &ws.WeatherPlace,
		&ws.WeatherLat, &ws.WeatherLon, &ws.WeatherUnits, &ws.WebSearchEnabled, &ws.WebSearchURL, &ws.KidWebSearch)
	return ws, err
}

// WidgetSettings reads the widget settings; a missing row is the defaults
// (everything off, °C, DuckDuckGo).
func (s *Store) WidgetSettings(ctx context.Context) (WidgetSettings, error) {
	ws, err := scanWidgetSettings(s.pool.QueryRow(ctx,
		`SELECT `+widgetSettingsColumns+` FROM system_settings WHERE id = 1`))
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultWidgetSettings(), nil
	}
	return ws, err
}

// UpsertWidgetSettings writes every widget setting and returns what was
// stored. The caller merges a partial PATCH first.
func (s *Store) UpsertWidgetSettings(ctx context.Context, in WidgetSettings) (WidgetSettings, error) {
	return scanWidgetSettings(s.pool.QueryRow(ctx,
		`INSERT INTO system_settings (id, clock_enabled, clock_hour12, weather_enabled, weather_place,
		    weather_lat, weather_lon, weather_units, web_search_enabled, web_search_url, kid_web_search, updated_at)
		 VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
		 ON CONFLICT (id) DO UPDATE SET
		   clock_enabled = EXCLUDED.clock_enabled, clock_hour12 = EXCLUDED.clock_hour12,
		   weather_enabled = EXCLUDED.weather_enabled, weather_place = EXCLUDED.weather_place,
		   weather_lat = EXCLUDED.weather_lat, weather_lon = EXCLUDED.weather_lon,
		   weather_units = EXCLUDED.weather_units, web_search_enabled = EXCLUDED.web_search_enabled,
		   web_search_url = EXCLUDED.web_search_url, kid_web_search = EXCLUDED.kid_web_search,
		   updated_at = now()
		 RETURNING `+widgetSettingsColumns,
		in.ClockEnabled, in.ClockHour12, in.WeatherEnabled, in.WeatherPlace, in.WeatherLat, in.WeatherLon,
		in.WeatherUnits, in.WebSearchEnabled, in.WebSearchURL, in.KidWebSearch))
}

// SetShowWidgets updates one user's "Show widgets" switch (AC-031).
func (s *Store) SetShowWidgets(ctx context.Context, userID string, show bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE users SET show_widgets = $2 WHERE id = $1`, userID, show)
	if isNoRowsOrBadID(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
