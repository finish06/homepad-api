package storage_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// SPEC-status-widgets §9 — migration 0020_widgets: service_widgets, the
// widget settings on system_settings, and users.show_widgets. Migrations
// re-run on every boot, so 0020 must upgrade a pre-0020 database in place and
// then be a no-op.

func widgetStore(t *testing.T) (*storage.Store, *pgx.Conn, string) {
	t.Helper()
	dsn := requireDatabaseURL(t)
	ctx := context.Background()
	store, err := storage.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	require.NoError(t, store.Migrate(ctx))
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close(ctx) })
	_, err = conn.Exec(ctx, `TRUNCATE categories, users, system_settings CASCADE`)
	require.NoError(t, err)
	var admin string
	require.NoError(t, conn.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, role) VALUES ('w20@example.com', 'h', 'admin') RETURNING id`).Scan(&admin))
	return store, conn, admin
}

func TestMigration0020_UpgradesInPlaceAndIsIdempotent(t *testing.T) {
	store, conn, admin := widgetStore(t)
	ctx := context.Background()

	// PRE-0020 shape.
	down, err := os.ReadFile("../../migrations/0020_widgets.down.sql")
	require.NoError(t, err, "0020 ships a down migration")
	_, err = conn.Exec(ctx, string(down))
	require.NoError(t, err, "the down migration runs")
	var n int
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'service_widgets'`).Scan(&n))
	require.Zero(t, n, "down drops service_widgets")
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'show_widgets'`).Scan(&n))
	require.Zero(t, n, "down drops users.show_widgets")

	// First run — the upgrade; existing users get show_widgets = true.
	require.NoError(t, store.Migrate(ctx))
	u, err := store.UserByID(ctx, admin)
	require.NoError(t, err)
	assert.True(t, u.ShowWidgets, "on by default for existing users (AC-031)")

	svc, err := store.CreateService(ctx, admin, storage.Service{Slug: "jf", Name: "Jellyfin", URL: "http://jellyfin.home.arpa:8096"})
	require.NoError(t, err)
	_, err = store.PutWidget(ctx, storage.Widget{ServiceID: svc.ID, Kind: "jellyfin", Fields: []string{"playing"}, IntervalS: 60, Enabled: true})
	require.NoError(t, err)

	// Every later boot — a no-op that keeps the widget.
	require.NoError(t, store.Migrate(ctx))
	require.NoError(t, store.Migrate(ctx))
	w, err := store.Widget(ctx, svc.ID)
	require.NoError(t, err)
	assert.Equal(t, "jellyfin", w.Kind)
}

func TestMigration0020_Constraints(t *testing.T) {
	store, conn, admin := widgetStore(t)
	ctx := context.Background()
	svc, err := store.CreateService(ctx, admin, storage.Service{Slug: "c20", Name: "C", URL: "https://c.example.com"})
	require.NoError(t, err)

	for name, stmt := range map[string]string{
		"interval not 60/300/900":    `INSERT INTO service_widgets (service_id, kind, interval_s) VALUES ($1, 'sonarr', 120)`,
		"more than 3 fields":         `INSERT INTO service_widgets (service_id, kind, fields) VALUES ($1, 'sonarr', '{a,b,c,d}')`,
		"ciphertext without a nonce": `INSERT INTO service_widgets (service_id, kind, secret_ciphertext) VALUES ($1, 'sonarr', '\x01')`,
		"nonce without a ciphertext": `INSERT INTO service_widgets (service_id, kind, secret_nonce) VALUES ($1, 'sonarr', '\x01')`,
	} {
		_, err := conn.Exec(ctx, stmt, svc.ID)
		assert.Error(t, err, name)
	}
	_, err = conn.Exec(ctx, `INSERT INTO system_settings (id, weather_units) VALUES (1, 'k')`)
	assert.Error(t, err, "weather units not c/f")

	// Defaults.
	_, err = conn.Exec(ctx, `INSERT INTO service_widgets (service_id, kind) VALUES ($1, 'sonarr')`, svc.ID)
	require.NoError(t, err)
	w, err := store.Widget(ctx, svc.ID)
	require.NoError(t, err)
	assert.Equal(t, 60, w.IntervalS)
	assert.True(t, w.Enabled)
	assert.False(t, w.InsecureTLS)
	assert.Empty(t, w.Address)
	assert.Empty(t, w.Fields)
	assert.Nil(t, w.SecretCiphertext)

	// AC-023 — deleting a service deletes its widget (FK cascade).
	require.NoError(t, store.DeleteService(ctx, svc.ID, admin))
	_, err = store.Widget(ctx, svc.ID)
	assert.True(t, errors.Is(err, storage.ErrNotFound))
	var n int
	require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM service_widgets`).Scan(&n))
	assert.Zero(t, n)
}

func TestWidgets_PutGetListDelete(t *testing.T) {
	store, conn, admin := widgetStore(t)
	ctx := context.Background()
	a, err := store.CreateService(ctx, admin, storage.Service{Slug: "son", Name: "Sonarr", URL: "http://sonarr.home.arpa:8989"})
	require.NoError(t, err)
	b, err := store.CreateService(ctx, admin, storage.Service{Slug: "rad", Name: "Radarr", URL: "http://radarr.home.arpa:7878"})
	require.NoError(t, err)

	ct := []byte{0xde, 0xad, 0xbe, 0xef}
	nonce := bytes.Repeat([]byte{7}, 12)
	got, err := store.PutWidget(ctx, storage.Widget{
		ServiceID: a.ID, Kind: "sonarr", Address: "http://10.0.0.5:8989", Username: "",
		SecretCiphertext: ct, SecretNonce: nonce, Fields: []string{"upcoming", "queued"},
		IntervalS: 300, InsecureTLS: true, Enabled: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "http://sonarr.home.arpa:8989", got.ServiceURL, "the tile URL rides along")
	assert.Equal(t, []string{"upcoming", "queued"}, got.Fields)
	assert.Equal(t, ct, got.SecretCiphertext)
	assert.Equal(t, nonce, got.SecretNonce)
	assert.Equal(t, 300, got.IntervalS)
	assert.True(t, got.InsecureTLS)

	// Upsert replaces the whole row.
	_, err = store.PutWidget(ctx, storage.Widget{ServiceID: a.ID, Kind: "sonarr", Fields: []string{"missing"}, IntervalS: 60, Enabled: true})
	require.NoError(t, err)
	got, err = store.Widget(ctx, a.ID)
	require.NoError(t, err)
	assert.Nil(t, got.SecretCiphertext)
	assert.Nil(t, got.SecretNonce)
	assert.Empty(t, got.Address)
	assert.Equal(t, []string{"missing"}, got.Fields)

	_, err = store.PutWidget(ctx, storage.Widget{ServiceID: b.ID, Kind: "radarr", IntervalS: 60, Enabled: false})
	require.NoError(t, err)
	list, err := store.EnabledWidgets(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1, "disabled widgets aren't fetched")
	assert.Equal(t, a.ID, list[0].ServiceID)
	assert.Equal(t, "http://sonarr.home.arpa:8989", list[0].ServiceURL)

	_, err = store.PutWidget(ctx, storage.Widget{ServiceID: "00000000-0000-0000-0000-000000000000", Kind: "sonarr", IntervalS: 60})
	assert.True(t, errors.Is(err, storage.ErrNotFound), "no such service")
	_, err = store.Widget(ctx, "not-a-uuid")
	assert.True(t, errors.Is(err, storage.ErrNotFound))

	require.NoError(t, store.DeleteWidget(ctx, a.ID))
	require.NoError(t, store.DeleteWidget(ctx, a.ID), "delete is idempotent")
	_, err = store.Widget(ctx, a.ID)
	assert.True(t, errors.Is(err, storage.ErrNotFound))

	var updated bool
	require.NoError(t, conn.QueryRow(ctx, `SELECT updated_at >= created_at FROM service_widgets WHERE service_id = $1`, b.ID).Scan(&updated))
	assert.True(t, updated)
}

func TestWidgetSettings_DefaultsAndRoundTrip(t *testing.T) {
	store, _, _ := widgetStore(t)
	ctx := context.Background()

	ws, err := store.WidgetSettings(ctx)
	require.NoError(t, err)
	assert.False(t, ws.ClockEnabled, "clock starts off (AC-035)")
	assert.Nil(t, ws.ClockHour12)
	assert.False(t, ws.WeatherEnabled, "weather starts off (AC-035)")
	assert.Equal(t, "c", ws.WeatherUnits)
	assert.Nil(t, ws.WeatherLat)
	assert.False(t, ws.WebSearchEnabled)
	assert.Equal(t, "https://duckduckgo.com/?q={query}", ws.WebSearchURL)
	assert.False(t, ws.KidWebSearch)

	// The cap6 settings and the widget settings don't clobber each other.
	ms := 1500
	_, err = store.UpsertSystemSettings(ctx, storage.SystemSettings{ShowUptimeDisplay: false, StatusDegradedMs: &ms})
	require.NoError(t, err)

	h12 := true
	lat, lon := 51.9225, 4.47917
	saved, err := store.UpsertWidgetSettings(ctx, storage.WidgetSettings{
		ClockEnabled: true, ClockHour12: &h12,
		WeatherEnabled: true, WeatherPlace: "Rotterdam, Netherlands", WeatherLat: &lat, WeatherLon: &lon, WeatherUnits: "f",
		WebSearchEnabled: true, WebSearchURL: "https://kagi.com/search?q={query}", KidWebSearch: true,
	})
	require.NoError(t, err)
	assert.True(t, saved.ClockEnabled)
	require.NotNil(t, saved.ClockHour12)
	assert.True(t, *saved.ClockHour12)
	require.NotNil(t, saved.WeatherLat)
	assert.Equal(t, 51.92, *saved.WeatherLat, "stored rounded to 2 decimals (AC-028)")
	assert.Equal(t, 4.48, *saved.WeatherLon)
	assert.Equal(t, "f", saved.WeatherUnits)
	assert.Equal(t, "https://kagi.com/search?q={query}", saved.WebSearchURL)
	assert.True(t, saved.KidWebSearch)

	sys, err := store.SystemSettings(ctx)
	require.NoError(t, err)
	assert.False(t, sys.ShowUptimeDisplay)
	require.NotNil(t, sys.StatusDegradedMs)
	assert.Equal(t, 1500, *sys.StatusDegradedMs)

	_, err = store.UpsertSystemSettings(ctx, storage.SystemSettings{ShowUptimeDisplay: true})
	require.NoError(t, err)
	again, err := store.WidgetSettings(ctx)
	require.NoError(t, err)
	assert.Equal(t, saved, again)

	saved.ClockHour12 = nil
	saved.WeatherLat, saved.WeatherLon = nil, nil
	after, err := store.UpsertWidgetSettings(ctx, saved)
	require.NoError(t, err)
	assert.Nil(t, after.ClockHour12, "null = the device's choice")
	assert.Nil(t, after.WeatherLat)
}

func TestShowWidgets_PerUser(t *testing.T) {
	store, _, admin := widgetStore(t)
	ctx := context.Background()
	u, err := store.CreateUser(ctx, "w20-user@example.com", "h", "user")
	require.NoError(t, err)
	assert.True(t, u.ShowWidgets)

	require.NoError(t, store.SetShowWidgets(ctx, u.ID, false))
	got, err := store.UserByID(ctx, u.ID)
	require.NoError(t, err)
	assert.False(t, got.ShowWidgets)
	other, err := store.UserByID(ctx, admin)
	require.NoError(t, err)
	assert.True(t, other.ShowWidgets, "another user is unchanged (TC-011)")

	assert.True(t, errors.Is(store.SetShowWidgets(ctx, "00000000-0000-0000-0000-000000000000", true), storage.ErrNotFound))
}
