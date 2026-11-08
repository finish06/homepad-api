package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/widgets"
)

// Status widgets (SPEC-status-widgets §7). The API server talks to the
// upstream app; the browser gets typed numbers on GET /api/services and never
// an address, a username or a key.
//
// Route classes (server.go, routes_test.go): GET /api/widgets/info is
// catalog-free (the clock and weather strip, which kids see too); every
// widget config route is admin-only.

// --- the store adapter for the widget manager ------------------------------

type widgetSource struct{ store *storage.Store }

// NewWidgetSource adapts the store to the widget manager's Source.
func NewWidgetSource(store *storage.Store) widgets.Source {
	if store == nil {
		return emptySource{}
	}
	return widgetSource{store: store}
}

type emptySource struct{}

func (emptySource) EnabledWidgets(context.Context) ([]widgets.Stored, error) { return nil, nil }
func (emptySource) Widget(context.Context, string) (widgets.Stored, bool, error) {
	return widgets.Stored{}, false, nil
}

func toStored(w storage.Widget) widgets.Stored {
	return widgets.Stored{
		ServiceID: w.ServiceID, Kind: w.Kind, Address: w.Address, ServiceURL: w.ServiceURL,
		Username: w.Username, SecretCiphertext: w.SecretCiphertext, SecretNonce: w.SecretNonce,
		Fields: w.Fields, Interval: time.Duration(w.IntervalS) * time.Second,
		InsecureTLS: w.InsecureTLS, Enabled: w.Enabled,
	}
}

func (s widgetSource) EnabledWidgets(ctx context.Context) ([]widgets.Stored, error) {
	rows, err := s.store.EnabledWidgets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]widgets.Stored, 0, len(rows))
	for _, r := range rows {
		out = append(out, toStored(r))
	}
	return out, nil
}

func (s widgetSource) Widget(ctx context.Context, serviceID string) (widgets.Stored, bool, error) {
	w, err := s.store.Widget(ctx, serviceID)
	if errors.Is(err, storage.ErrNotFound) {
		return widgets.Stored{}, false, nil
	}
	if err != nil {
		return widgets.Stored{}, false, err
	}
	return toStored(w), true, nil
}

// LoadWeatherSettings reads the weather settings for the weather fetcher.
func LoadWeatherSettings(ctx context.Context, store *storage.Store) (widgets.WeatherSettings, error) {
	ws, err := store.WidgetSettings(ctx)
	if err != nil {
		return widgets.WeatherSettings{}, err
	}
	return weatherSettings(ws), nil
}

func weatherSettings(ws storage.WidgetSettings) widgets.WeatherSettings {
	return widgets.WeatherSettings{
		Enabled: ws.WeatherEnabled, Place: ws.WeatherPlace, Lat: ws.WeatherLat, Lon: ws.WeatherLon, Units: ws.WeatherUnits,
	}
}

// --- wire views ------------------------------------------------------------

// tileWidgetView is the `widget` field on GET /api/services (§7.2): kind,
// state, asOf and the chosen fields as typed values. It has no field that
// could carry an address, a username, a key or an error code (AC-002, AC-013).
type tileWidgetView struct {
	Kind   string          `json:"kind"`
	State  string          `json:"state"`
	AsOf   *time.Time      `json:"asOf"`
	Fields []tileFieldView `json:"fields"`
}

type tileFieldView struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Unit  string   `json:"unit"`
	Value float64  `json:"value"`
	Total *float64 `json:"total,omitempty"`
}

func fieldViews(fs []widgets.FieldValue) []tileFieldView {
	out := make([]tileFieldView, 0, len(fs))
	for _, f := range fs {
		out = append(out, tileFieldView{Key: f.Key, Label: f.Label, Unit: string(f.Unit), Value: f.Value, Total: f.Total})
	}
	return out
}

// tileWidget is the tile view of a manager view. loading and error carry no
// fields (AC-007, AC-013).
func tileWidget(v widgets.View) *tileWidgetView {
	tv := &tileWidgetView{Kind: v.Kind, State: v.State, AsOf: v.AsOf, Fields: []tileFieldView{}}
	if v.State == widgets.StateOK || v.State == widgets.StateStale {
		tv.Fields = fieldViews(v.Fields)
	}
	return tv
}

// widgetConfigView is the admin's view of a widget (GET/PUT
// /api/services/{id}/widget). The key is write-only: only hasSecret.
type widgetConfigView struct {
	Kind        string     `json:"kind"`
	Address     string     `json:"address"`
	Username    string     `json:"username"`
	HasSecret   bool       `json:"hasSecret"`
	Fields      []string   `json:"fields"`
	IntervalS   int        `json:"intervalS"`
	InsecureTLS bool       `json:"insecureTls"`
	Enabled     bool       `json:"enabled"`
	State       string     `json:"state"`
	Error       string     `json:"error"`
	LastOkAt    *time.Time `json:"lastOkAt"`
	LastCheckAt *time.Time `json:"lastCheckAt"`
}

func (s *server) widgetConfig(w storage.Widget) widgetConfigView {
	v := widgetConfigView{
		Kind: w.Kind, Address: w.Address, Username: w.Username, HasSecret: w.SecretCiphertext != nil,
		Fields: w.Fields, IntervalS: w.IntervalS, InsecureTLS: w.InsecureTLS, Enabled: w.Enabled,
		State: widgets.StateLoading,
	}
	if v.Fields == nil {
		v.Fields = []string{}
	}
	if mv, ok := s.widgets.View(w.ServiceID); ok {
		v.State, v.Error, v.LastOkAt, v.LastCheckAt = mv.State, string(mv.LastError), mv.AsOf, mv.LastCheck
	}
	return v
}

// --- input -----------------------------------------------------------------

const maxWidgetBody = 16 << 10
const maxSecretLen = 4096

// widgetInput is the PUT (and test) body. Secret: omitted = keep, "" =
// clear, anything else = replace (§6.4).
type widgetInput struct {
	Kind        string   `json:"kind"`
	Address     string   `json:"address"`
	Username    string   `json:"username"`
	Secret      *string  `json:"secret"`
	Fields      []string `json:"fields"`
	IntervalS   int      `json:"intervalS"`
	InsecureTLS bool     `json:"insecureTls"`
	Enabled     *bool    `json:"enabled"`
}

// fieldError is a 422 with the field it is about and plain copy for it.
type fieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func writeFieldError(w http.ResponseWriter, fe *fieldError) {
	writeJSON(w, http.StatusUnprocessableEntity, fe)
}

// validWidgetAddress reports whether raw is "" (use the tile's link) or an
// absolute http(s) URL with a host and no user:password part.
func validWidgetAddress(raw string) *fieldError {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(raw, " \t\r\n") {
		return &fieldError{"address", "Use a full address that starts with http:// or https://."}
	}
	if u.User != nil {
		return &fieldError{"address", "Put the key in the key field, not in the address."}
	}
	return nil
}

// validate checks a widget body and normalises its fields to the provider's
// order. forSave adds the checks that only matter when storing (a key with
// no server key).
func (s *server) validateWidget(in *widgetInput, forSave bool) (widgets.Provider, *fieldError) {
	in.Kind = strings.TrimSpace(in.Kind)
	in.Address = strings.TrimSpace(in.Address)
	in.Username = strings.TrimSpace(in.Username)
	p, ok := s.widgetKinds.Get(in.Kind)
	if !ok {
		return nil, &fieldError{"kind", "Choose a widget type."}
	}
	if len(in.Fields) > 3 {
		return nil, &fieldError{"fields", "Choose up to 3 numbers."}
	}
	fields, unknown := widgets.NormalizeFields(p, in.Fields)
	if unknown != "" {
		return nil, &fieldError{"fields", "That number isn't available for this type."}
	}
	in.Fields = fields
	if in.IntervalS == 0 {
		in.IntervalS = 60
	}
	if in.IntervalS != 60 && in.IntervalS != 300 && in.IntervalS != 900 {
		return nil, &fieldError{"intervalS", "Refresh must be every 1, 5 or 15 minutes."}
	}
	if fe := validWidgetAddress(in.Address); fe != nil {
		return nil, fe
	}
	if len(in.Username) > 256 {
		return nil, &fieldError{"username", "That name is too long."}
	}
	if in.Secret != nil && len(*in.Secret) > maxSecretLen {
		return nil, &fieldError{"secret", "That key is too long."}
	}
	if forSave && in.Secret != nil && *in.Secret != "" && s.sealer == nil {
		return nil, &fieldError{"secret", "To save keys, set HOMEPAD_SECRET_KEY on the server."}
	}
	return p, nil
}

func decodeWidgetInput(w http.ResponseWriter, r *http.Request) (widgetInput, bool) {
	var in widgetInput
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWidgetBody))
	if err != nil || json.Unmarshal(raw, &in) != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return widgetInput{}, false
	}
	return in, true
}

// widgetService resolves {id} to a shared-catalog service and its tile URL.
// On failure it has written the response.
func (s *server) widgetService(w http.ResponseWriter, r *http.Request) (id, tileURL string, ok bool) {
	owner, ok := s.catalogOwner(w, r, "no such service")
	if !ok {
		return "", "", false
	}
	id = r.PathValue("id")
	tileURL, err := s.store.ServiceURL(r.Context(), id, owner)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such service", http.StatusNotFound)
		return "", "", false
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return "", "", false
	}
	return id, tileURL, true
}

func effectiveAddress(address, tileURL string) string {
	if address != "" {
		return address
	}
	return tileURL
}

// keepsSavedKey reports whether a saved key may stay with the new config:
// only when the kind and the origin (scheme, host, port) are unchanged
// (AC-023), so a key is never sent to a host it wasn't entered for.
func keepsSavedKey(cur storage.Widget, kind, address, tileURL string) bool {
	if cur.SecretCiphertext == nil || cur.Kind != kind {
		return false
	}
	return widgets.Origin(effectiveAddress(cur.Address, cur.ServiceURL)) == widgets.Origin(effectiveAddress(address, tileURL))
}

// --- handlers --------------------------------------------------------------

type kindView struct {
	Kind   string              `json:"kind"`
	Label  string              `json:"label"`
	Auth   widgets.AuthType    `json:"auth"`
	Fields []widgets.FieldSpec `json:"fields"`
	Match  []string            `json:"match"`
	Help   string              `json:"help"`
}

// handleWidgetKinds lists the provider registry for the editor, and whether
// the server can store keys (HOMEPAD_SECRET_KEY set).
func (s *server) handleWidgetKinds(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	kinds := []kindView{}
	for _, p := range s.widgetKinds.All() {
		kinds = append(kinds, kindView{Kind: p.Kind(), Label: p.Label(), Auth: p.Auth(), Fields: p.Fields(), Match: p.Match(), Help: p.Help()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"secretKeySet": s.sealer != nil, "kinds": kinds})
}

func (s *server) handleGetWidget(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	id, _, ok := s.widgetService(w, r)
	if !ok {
		return
	}
	cur, err := s.store.Widget(r.Context(), id)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no widget", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s.widgetConfig(cur))
}

// handlePutWidget whole-replaces a service's widget config, except the key,
// which follows §6.4 (omitted = keep, "" = clear, else replace) and is
// cleared when the kind or the address origin changes (AC-023).
func (s *server) handlePutWidget(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	id, tileURL, ok := s.widgetService(w, r)
	if !ok {
		return
	}
	in, ok := decodeWidgetInput(w, r)
	if !ok {
		return
	}
	if _, fe := s.validateWidget(&in, true); fe != nil {
		writeFieldError(w, fe)
		return
	}
	cur, err := s.store.Widget(r.Context(), id)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	next := storage.Widget{
		ServiceID: id, Kind: in.Kind, Address: in.Address, Username: in.Username, Fields: in.Fields,
		IntervalS: in.IntervalS, InsecureTLS: in.InsecureTLS, Enabled: in.Enabled == nil || *in.Enabled,
	}
	switch {
	case in.Secret != nil && *in.Secret == "":
		// cleared
	case in.Secret != nil:
		ct, nonce, err := s.sealer.Seal([]byte(*in.Secret), widgets.SecretAAD(id, effectiveAddress(in.Address, tileURL)))
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		next.SecretCiphertext, next.SecretNonce = ct, nonce
	case keepsSavedKey(cur, in.Kind, in.Address, tileURL):
		next.SecretCiphertext, next.SecretNonce = cur.SecretCiphertext, cur.SecretNonce
	}
	saved, err := s.store.PutWidget(r.Context(), next)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such service", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.widgets.Reload(r.Context(), id); err != nil {
		log.Printf("widget %s: reload failed", id)
	}
	writeJSON(w, http.StatusOK, s.widgetConfig(saved))
}

func (s *server) handleDeleteWidget(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	id, _, ok := s.widgetService(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteWidget(r.Context(), id); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.widgets.Remove(id)
	w.WriteHeader(http.StatusNoContent)
}

// testInterval is the per-admin Test connection limit (AC-021), so the
// button can't be used as a fast port scanner.
const testInterval = 2 * time.Second

type testLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (l *testLimiter) allow(userID string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	if t, ok := l.last[userID]; ok && now.Sub(t) < testInterval {
		return false
	}
	l.last[userID] = now
	return true
}

// handleTestWidget tests the form's UNSAVED values (AC-021). A blank key
// means "use the saved key", unless the kind or origin changed. The answer is
// OK with the version and values, or one error code; never a body or text.
func (s *server) handleTestWidget(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	id, tileURL, ok := s.widgetService(w, r)
	if !ok {
		return
	}
	in, ok := decodeWidgetInput(w, r)
	if !ok {
		return
	}
	if _, fe := s.validateWidget(&in, false); fe != nil {
		writeFieldError(w, fe)
		return
	}
	if !s.tests.allow(u.ID, time.Now()) {
		http.Error(w, "Wait a moment, then test again.", http.StatusTooManyRequests)
		return
	}
	address := effectiveAddress(in.Address, tileURL)
	base, err := url.Parse(address)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": widgets.CodeBlockedAddress})
		return
	}
	secret := ""
	switch {
	case in.Secret != nil:
		secret = *in.Secret
	default:
		cur, err := s.store.Widget(r.Context(), id)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if keepsSavedKey(cur, in.Kind, in.Address, tileURL) {
			plain, err := s.sealer.Open(cur.SecretCiphertext, cur.SecretNonce, widgets.SecretAAD(id, effectiveAddress(cur.Address, cur.ServiceURL)))
			if err != nil {
				writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": widgets.CodeSecretUnreadable})
				return
			}
			secret = string(plain)
		}
	}
	fields, version, terr := s.widgets.Test(r.Context(), id, widgets.TestInput{
		Kind: in.Kind, BaseURL: base, Username: in.Username, Secret: secret, InsecureTLS: in.InsecureTLS, Fields: in.Fields,
	})
	if terr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": widgets.CodeOf(terr)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version, "fields": fieldViews(fields)})
}

// handleGeocode is the admin's place search, through the server so the
// browser never calls Open-Meteo (AC-028).
func (s *server) handleGeocode(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) < 2 || len(q) > 200 {
		writeFieldError(w, &fieldError{"q", "Type at least 2 letters."})
		return
	}
	places, err := s.weather.Geocode(r.Context(), q)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": widgets.CodeUnreachable})
		return
	}
	writeJSON(w, http.StatusOK, places)
}

// searchProviders names the web search presets by host (§7.3).
var searchProviders = map[string]string{
	"duckduckgo.com": "DuckDuckGo", "www.duckduckgo.com": "DuckDuckGo",
	"google.com": "Google", "www.google.com": "Google",
	"bing.com": "Bing", "www.bing.com": "Bing",
	"kagi.com":      "Kagi",
	"startpage.com": "Startpage", "www.startpage.com": "Startpage",
}

func searchName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if name, ok := searchProviders[host]; ok {
		return name
	}
	return host
}

// handleWidgetsInfo serves the glance strip and the ⌘K web search config.
// It needs a session and holds no catalog data, so kids read it too
// (catalog-free, AC-029). Stored coordinates are never returned.
func (s *server) handleWidgetsInfo(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(w, r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ws, err := s.store.WidgetSettings(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	weather := map[string]any{"enabled": ws.WeatherEnabled, "place": ws.WeatherPlace, "units": ws.WeatherUnits}
	if ws.WeatherEnabled {
		v := s.weather.View()
		weather["state"], weather["asOf"] = v.State, v.AsOf
		if v.Reading != nil && (v.State == widgets.StateOK || v.State == widgets.StateStale) {
			weather["temp"], weather["high"], weather["low"] = v.Reading.Temp, v.Reading.High, v.Reading.Low
			weather["condition"], weather["isDay"] = v.Reading.Condition, v.Reading.IsDay
		}
	} else {
		weather["state"], weather["asOf"] = widgets.StateError, nil
	}
	search := map[string]any{
		"enabled": ws.WebSearchEnabled && (storage.IsAdultRole(u.Role) || ws.KidWebSearch),
		"name":    searchName(ws.WebSearchURL),
		"url":     ws.WebSearchURL,
	}
	if u.Role == "admin" {
		search["kids"] = ws.KidWebSearch
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"clock":   map[string]any{"enabled": ws.ClockEnabled, "hour12": ws.ClockHour12},
		"weather": weather,
		"search":  search,
	})
}

// --- settings PATCH --------------------------------------------------------

// optionalBool distinguishes a JSON key that is absent, null, or a boolean.
type optionalBool struct {
	Set   bool
	Value *bool
}

func (o *optionalBool) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v bool
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// widgetSettingsPatch is the widget half of PATCH /api/admin/settings. Every
// field is optional; only present ones change.
type widgetSettingsPatch struct {
	ClockEnabled     *bool        `json:"clockEnabled"`
	ClockHour12      optionalBool `json:"clockHour12"`
	WeatherEnabled   *bool        `json:"weatherEnabled"`
	WeatherPlace     *string      `json:"weatherPlace"`
	WeatherLat       *float64     `json:"weatherLat"`
	WeatherLon       *float64     `json:"weatherLon"`
	WeatherUnits     *string      `json:"weatherUnits"`
	WebSearchEnabled *bool        `json:"webSearchEnabled"`
	WebSearchURL     *string      `json:"webSearchUrl"`
	KidWebSearch     *bool        `json:"kidWebSearch"`
}

func (p widgetSettingsPatch) any() bool {
	return p.ClockEnabled != nil || p.ClockHour12.Set || p.WeatherEnabled != nil || p.WeatherPlace != nil ||
		p.WeatherLat != nil || p.WeatherLon != nil || p.WeatherUnits != nil || p.WebSearchEnabled != nil ||
		p.WebSearchURL != nil || p.KidWebSearch != nil
}

// apply merges the patch over cur and validates the result.
func (p widgetSettingsPatch) apply(cur storage.WidgetSettings) (storage.WidgetSettings, *fieldError) {
	next := cur
	if p.ClockEnabled != nil {
		next.ClockEnabled = *p.ClockEnabled
	}
	if p.ClockHour12.Set {
		next.ClockHour12 = p.ClockHour12.Value
	}
	if p.WeatherUnits != nil {
		if *p.WeatherUnits != "c" && *p.WeatherUnits != "f" {
			return cur, &fieldError{"weatherUnits", "Units must be °C or °F."}
		}
		next.WeatherUnits = *p.WeatherUnits
	}
	if p.WeatherLat != nil || p.WeatherLon != nil {
		if p.WeatherLat == nil || p.WeatherLon == nil {
			return cur, &fieldError{"weatherLat", "Choose a place from the search results."}
		}
		lat, lon := *p.WeatherLat, *p.WeatherLon
		if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
			return cur, &fieldError{"weatherLat", "Choose a place from the search results."}
		}
		lat, lon = widgets.RoundCoord(lat), widgets.RoundCoord(lon)
		next.WeatherLat, next.WeatherLon = &lat, &lon
	}
	if p.WeatherPlace != nil {
		place := strings.TrimSpace(*p.WeatherPlace)
		if len(place) > 200 {
			return cur, &fieldError{"weatherPlace", "That place name is too long."}
		}
		next.WeatherPlace = place
	}
	if p.WeatherEnabled != nil {
		next.WeatherEnabled = *p.WeatherEnabled
	}
	if next.WeatherEnabled && (next.WeatherLat == nil || next.WeatherLon == nil) {
		return cur, &fieldError{"weatherEnabled", "Choose a place first."}
	}
	if p.WebSearchURL != nil {
		raw := strings.TrimSpace(*p.WebSearchURL)
		u, err := url.Parse(raw)
		switch {
		case err != nil || u.Scheme != "https" || u.Host == "":
			return cur, &fieldError{"webSearchUrl", "The address must start with https://."}
		case !strings.Contains(raw, "{query}"):
			return cur, &fieldError{"webSearchUrl", "The address must contain {query}."}
		case len(raw) > 500:
			return cur, &fieldError{"webSearchUrl", "That address is too long."}
		}
		next.WebSearchURL = raw
	}
	if p.WebSearchEnabled != nil {
		next.WebSearchEnabled = *p.WebSearchEnabled
	}
	if p.KidWebSearch != nil {
		next.KidWebSearch = *p.KidWebSearch
	}
	return next, nil
}

// widgetSettingsView is the `widgets` block of the PATCH response (admin
// only). The coordinates are not echoed; the place name says what was chosen.
type widgetSettingsView struct {
	ClockEnabled     bool   `json:"clockEnabled"`
	ClockHour12      *bool  `json:"clockHour12"`
	WeatherEnabled   bool   `json:"weatherEnabled"`
	WeatherPlace     string `json:"weatherPlace"`
	WeatherUnits     string `json:"weatherUnits"`
	WebSearchEnabled bool   `json:"webSearchEnabled"`
	WebSearchURL     string `json:"webSearchUrl"`
	KidWebSearch     bool   `json:"kidWebSearch"`
}

func toWidgetSettingsView(ws storage.WidgetSettings) widgetSettingsView {
	return widgetSettingsView{
		ClockEnabled: ws.ClockEnabled, ClockHour12: ws.ClockHour12, WeatherEnabled: ws.WeatherEnabled,
		WeatherPlace: ws.WeatherPlace, WeatherUnits: ws.WeatherUnits, WebSearchEnabled: ws.WebSearchEnabled,
		WebSearchURL: ws.WebSearchURL, KidWebSearch: ws.KidWebSearch,
	}
}

// patchWidgetSettings applies the widget half of a settings PATCH. It
// returns the settings to echo, or a field error (nothing written).
func (s *server) patchWidgetSettings(ctx context.Context, raw []byte) (widgetSettingsView, *fieldError, error) {
	var p widgetSettingsPatch
	if err := json.Unmarshal(raw, &p); err != nil {
		return widgetSettingsView{}, &fieldError{"body", "Check the values and try again."}, nil
	}
	cur, err := s.store.WidgetSettings(ctx)
	if err != nil {
		return widgetSettingsView{}, nil, err
	}
	if !p.any() {
		return toWidgetSettingsView(cur), nil, nil
	}
	next, fe := p.apply(cur)
	if fe != nil {
		return widgetSettingsView{}, fe, nil
	}
	saved, err := s.store.UpsertWidgetSettings(ctx, next)
	if err != nil {
		return widgetSettingsView{}, nil, err
	}
	s.weather.Configure(weatherSettings(saved))
	return toWidgetSettingsView(saved), nil, nil
}
