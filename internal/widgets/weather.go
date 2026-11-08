package widgets

import (
	"context"
	"math"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Weather is the glance strip's weather (SPEC-status-widgets AC-028): fetched
// by the server from Open-Meteo every 15 min while weather is on, through the
// guarded client. The browser never calls Open-Meteo. Open-Meteo's terms ask
// for credit (CC BY 4.0) and non-commercial use under 10,000 calls a day;
// Homepad makes 96.
//
// States follow the UX doc (§4): a reading is current for 30 min; after that
// it is shown with a ◷ mark (stale); a reading older than StaleWeatherMax is
// not shown at all (error), so a server that lost its internet doesn't show
// yesterday's temperature as if it were now.

const (
	DefaultForecastURL = "https://api.open-meteo.com/v1/forecast"
	DefaultGeocodeURL  = "https://geocoding-api.open-meteo.com/v1/search"
	WeatherInterval    = 15 * time.Minute
	// StaleWeatherMax is the oldest reading the strip still shows.
	StaleWeatherMax = 3 * time.Hour
)

// WeatherSettings is the admin's weather config.
type WeatherSettings struct {
	Enabled bool
	Place   string
	Lat     *float64
	Lon     *float64
	Units   string // "c" or "f"
}

func (s WeatherSettings) ready() bool { return s.Enabled && s.Lat != nil && s.Lon != nil }

func (s WeatherSettings) sameReading(o WeatherSettings) bool {
	eq := func(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	return s.Units == o.Units && eq(s.Lat, o.Lat) && eq(s.Lon, o.Lon)
}

// WeatherOptions configures Weather. The URLs default to Open-Meteo; tests
// point them at httptest fakes.
type WeatherOptions struct {
	Client      *Client
	ForecastURL string
	GeocodeURL  string
	Now         func() time.Time
	Logf        func(format string, args ...any)
	Load        func(ctx context.Context) (WeatherSettings, error) // initial settings for Run
	Tick        time.Duration
}

// Reading is one forecast, already reduced to what the strip shows.
type Reading struct {
	Temp, High, Low float64
	Condition       string // one of 7 conditions (ConditionFor)
	IsDay           bool
}

// WeatherView is what the info endpoint reads.
type WeatherView struct {
	State   string
	AsOf    *time.Time
	Reading *Reading
}

// Place is one place-search result, coordinates rounded to 2 decimals.
type Place struct {
	Name    string  `json:"name"`
	Region  string  `json:"region"`
	Country string  `json:"country"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
}

// Weather fetches and caches the household's weather.
type Weather struct {
	client   *Client
	forecast string
	geocode  string
	now      func() time.Time
	logf     func(string, ...any)
	load     func(ctx context.Context) (WeatherSettings, error)
	tick     time.Duration
	wake     chan struct{}

	fetchMu sync.Mutex // one forecast fetch at a time

	mu        sync.Mutex
	cfg       WeatherSettings
	gen       int
	checked   bool
	lastCheck time.Time
	reading   *Reading
	lastOK    time.Time
}

// NewWeather builds a Weather.
func NewWeather(o WeatherOptions) *Weather {
	w := &Weather{
		client: o.Client, forecast: o.ForecastURL, geocode: o.GeocodeURL,
		now: o.Now, logf: o.Logf, load: o.Load, tick: o.Tick,
		wake: make(chan struct{}, 1),
	}
	if w.client == nil {
		w.client = NewClient()
	}
	if w.forecast == "" {
		w.forecast = DefaultForecastURL
	}
	if w.geocode == "" {
		w.geocode = DefaultGeocodeURL
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.logf == nil {
		w.logf = func(string, ...any) {}
	}
	if w.tick <= 0 {
		w.tick = 10 * time.Second
	}
	return w
}

// Run loads the settings, then fetches every WeatherInterval while weather is
// on, and at once after Configure changes the place or units.
func (w *Weather) Run(ctx context.Context) error {
	loaded := w.load == nil
	t := time.NewTicker(w.tick)
	defer t.Stop()
	for {
		if !loaded {
			if s, err := w.load(ctx); err == nil {
				w.Configure(s)
				loaded = true
			}
		}
		if w.due() {
			if err := w.RefreshNow(ctx); err != nil {
				w.logf("weather: %s", CodeOf(err))
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case <-w.wake:
		}
	}
}

func (w *Weather) due() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.cfg.ready() {
		return false
	}
	return !w.checked || !w.now().Before(w.lastCheck.Add(WeatherInterval))
}

// Configure applies new settings. A change of place or units drops the old
// reading and wakes Run for an immediate fetch.
func (w *Weather) Configure(s WeatherSettings) {
	w.mu.Lock()
	changed := !w.cfg.sameReading(s)
	w.cfg = s
	if changed {
		w.gen++
		w.checked = false
		w.reading = nil
	}
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Settings returns the current settings.
func (w *Weather) Settings() WeatherSettings {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cfg
}

type forecastBody struct {
	Current struct {
		Temp  *float64 `json:"temperature_2m"`
		Code  *int     `json:"weather_code"`
		IsDay *int     `json:"is_day"`
	} `json:"current"`
	Daily struct {
		Max []float64 `json:"temperature_2m_max"`
		Min []float64 `json:"temperature_2m_min"`
	} `json:"daily"`
}

// RefreshNow fetches the forecast now. With weather off or no place set it
// makes no request.
func (w *Weather) RefreshNow(ctx context.Context) error {
	w.fetchMu.Lock()
	defer w.fetchMu.Unlock()
	w.mu.Lock()
	cfg, gen := w.cfg, w.gen
	w.mu.Unlock()
	if !cfg.ready() {
		return nil
	}
	u, err := url.Parse(w.forecast)
	if err != nil {
		return ErrBadResponse
	}
	q := url.Values{}
	q.Set("latitude", strconv.FormatFloat(*cfg.Lat, 'f', 2, 64))
	q.Set("longitude", strconv.FormatFloat(*cfg.Lon, 'f', 2, 64))
	q.Set("current", "temperature_2m,weather_code,is_day")
	q.Set("daily", "temperature_2m_max,temperature_2m_min")
	q.Set("timezone", "auto")
	q.Set("forecast_days", "1")
	if cfg.Units == "f" {
		q.Set("temperature_unit", "fahrenheit")
	}
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(ctx, RefreshBudget)
	defer cancel()
	var body forecastBody
	ferr := w.client.GetJSON(ctx, u, nil, false, &body)
	var r *Reading
	if ferr == nil {
		if body.Current.Temp == nil || body.Current.Code == nil || len(body.Daily.Max) == 0 || len(body.Daily.Min) == 0 {
			ferr = ErrBadResponse
		} else {
			r = &Reading{
				Temp: *body.Current.Temp, High: body.Daily.Max[0], Low: body.Daily.Min[0],
				Condition: ConditionFor(*body.Current.Code),
				IsDay:     body.Current.IsDay == nil || *body.Current.IsDay == 1,
			}
		}
	}

	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.gen != gen {
		return ferr // settings changed mid-fetch; this reading is for the old place
	}
	w.checked = true
	w.lastCheck = now
	if r != nil {
		w.reading = r
		w.lastOK = now
	}
	return ferr
}

// View returns the weather state, computed at read time.
func (w *Weather) View() WeatherView {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.cfg.ready() {
		return WeatherView{State: StateError}
	}
	if w.reading != nil {
		age := now.Sub(w.lastOK)
		t := w.lastOK
		r := *w.reading
		switch {
		case age <= 2*WeatherInterval:
			return WeatherView{State: StateOK, AsOf: &t, Reading: &r}
		case age <= StaleWeatherMax:
			return WeatherView{State: StateStale, AsOf: &t, Reading: &r}
		}
		return WeatherView{State: StateError, AsOf: &t}
	}
	if !w.checked {
		return WeatherView{State: StateLoading}
	}
	return WeatherView{State: StateError}
}

type geocodeBody struct {
	Results []struct {
		Name    string   `json:"name"`
		Admin1  string   `json:"admin1"`
		Country string   `json:"country"`
		Lat     *float64 `json:"latitude"`
		Lon     *float64 `json:"longitude"`
	} `json:"results"`
}

// Geocode is the admin's place search (GET /api/admin/geocode), through the
// server so the browser never calls Open-Meteo. Up to 5 results.
func (w *Weather) Geocode(ctx context.Context, q string) ([]Place, error) {
	u, err := url.Parse(w.geocode)
	if err != nil {
		return nil, ErrBadResponse
	}
	v := url.Values{}
	v.Set("name", q)
	v.Set("count", "5")
	v.Set("language", "en")
	v.Set("format", "json")
	u.RawQuery = v.Encode()
	var body geocodeBody
	if err := w.client.GetJSON(ctx, u, nil, false, &body); err != nil {
		return nil, err
	}
	out := []Place{}
	for _, r := range body.Results {
		if r.Lat == nil || r.Lon == nil || len(out) == 5 {
			continue
		}
		out = append(out, Place{Name: r.Name, Region: r.Admin1, Country: r.Country, Lat: RoundCoord(*r.Lat), Lon: RoundCoord(*r.Lon)})
	}
	return out, nil
}

// ConditionFor maps a WMO weather code to one of the strip's 7 conditions.
func ConditionFor(code int) string {
	switch {
	case code == 0:
		return "clear"
	case code == 1 || code == 2:
		return "partly_cloudy"
	case code == 3:
		return "cloudy"
	case code == 45 || code == 48:
		return "fog"
	case (code >= 51 && code <= 67) || (code >= 80 && code <= 82):
		return "rain"
	case (code >= 71 && code <= 77) || code == 85 || code == 86:
		return "snow"
	case code >= 95 && code <= 99:
		return "storm"
	}
	return "cloudy"
}

// RoundCoord rounds a coordinate to 2 decimals (about 1 km), the precision
// stored (AC-028).
func RoundCoord(f float64) float64 {
	r := math.Round(f*100) / 100
	if r == 0 {
		return 0 // no "-0"
	}
	return r
}
