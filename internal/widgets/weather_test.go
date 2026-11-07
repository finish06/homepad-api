package widgets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-status-widgets AC-028 — weather is fetched by the server from Open-Meteo
// (here, an httptest fake of its forecast and geocoding APIs).

type fakeMeteo struct {
	srv       *httptest.Server
	forecasts atomic.Int32
	geocodes  atomic.Int32
	mu        sync.Mutex
	lastQuery url.Values
	code      int
	fail      bool
}

func newFakeMeteo(t *testing.T) *fakeMeteo {
	t.Helper()
	f := &fakeMeteo{code: 3}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.lastQuery = r.URL.Query()
		fail, code := f.fail, f.code
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case "/v1/forecast":
			f.forecasts.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"current": map[string]any{"temperature_2m": 7.7, "weather_code": code, "is_day": 0},
				"daily":   map[string]any{"temperature_2m_max": []float64{9.0}, "temperature_2m_min": []float64{2.8}},
			})
		case "/v1/search":
			f.geocodes.Add(1)
			if r.URL.Query().Get("name") == "Nowhereville" {
				_, _ = w.Write([]byte(`{"generationtime_ms":0.1}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
				{"name": "Rotterdam", "admin1": "South Holland", "country": "Netherlands", "latitude": 51.92250, "longitude": 4.47917},
				{"name": "Rotterdam", "admin1": "New York", "country": "United States", "latitude": 42.80091, "longitude": -73.97069},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMeteo) query() url.Values { f.mu.Lock(); defer f.mu.Unlock(); return f.lastQuery }

func newTestWeather(t *testing.T, f *fakeMeteo, clock *fakeClock) *Weather {
	t.Helper()
	return NewWeather(WeatherOptions{
		Client:      NewClient(),
		ForecastURL: f.srv.URL + "/v1/forecast",
		GeocodeURL:  f.srv.URL + "/v1/search",
		Now:         clock.Now,
	})
}

func fp(v float64) *float64 { return &v }

func TestWeather_DefaultsPointAtOpenMeteo(t *testing.T) {
	assert.Equal(t, "https://api.open-meteo.com/v1/forecast", DefaultForecastURL)
	assert.Equal(t, "https://geocoding-api.open-meteo.com/v1/search", DefaultGeocodeURL)
	assert.Equal(t, 15*time.Minute, WeatherInterval, "96 calls a day, under the 10,000/day terms")
}

func TestWeather_FetchesForecast(t *testing.T) {
	f := newFakeMeteo(t)
	clock := newFakeClock()
	w := newTestWeather(t, f, clock)
	w.Configure(WeatherSettings{Enabled: true, Place: "Rotterdam", Lat: fp(51.92), Lon: fp(4.48), Units: "c"})
	assert.Equal(t, StateLoading, w.View().State)

	require.NoError(t, w.RefreshNow(context.Background()))
	q := f.query()
	assert.Equal(t, "51.92", q.Get("latitude"))
	assert.Equal(t, "4.48", q.Get("longitude"))
	assert.Equal(t, "temperature_2m,weather_code,is_day", q.Get("current"))
	assert.Equal(t, "temperature_2m_max,temperature_2m_min", q.Get("daily"))
	assert.Equal(t, "auto", q.Get("timezone"))
	assert.Equal(t, "1", q.Get("forecast_days"))
	assert.Empty(t, q.Get("temperature_unit"), "°C is Open-Meteo's default")

	v := w.View()
	assert.Equal(t, StateOK, v.State)
	require.NotNil(t, v.Reading)
	assert.Equal(t, 7.7, v.Reading.Temp)
	assert.Equal(t, 9.0, v.Reading.High)
	assert.Equal(t, 2.8, v.Reading.Low)
	assert.Equal(t, "cloudy", v.Reading.Condition)
	assert.False(t, v.Reading.IsDay)
	require.NotNil(t, v.AsOf)

	w.Configure(WeatherSettings{Enabled: true, Place: "Rotterdam", Lat: fp(51.92), Lon: fp(4.48), Units: "f"})
	assert.Equal(t, StateLoading, w.View().State, "a units change starts over")
	require.NoError(t, w.RefreshNow(context.Background()))
	assert.Equal(t, "fahrenheit", f.query().Get("temperature_unit"))
}

func TestWeather_OffMakesNoRequest(t *testing.T) {
	f := newFakeMeteo(t)
	w := newTestWeather(t, f, newFakeClock())
	require.NoError(t, w.RefreshNow(context.Background()))
	w.Configure(WeatherSettings{Enabled: false, Place: "Rotterdam", Lat: fp(51.92), Lon: fp(4.48), Units: "c"})
	require.NoError(t, w.RefreshNow(context.Background()))
	w.Configure(WeatherSettings{Enabled: true, Units: "c"}) // no place yet
	require.NoError(t, w.RefreshNow(context.Background()))
	assert.Zero(t, f.forecasts.Load())
	assert.Nil(t, w.View().Reading)
}

func TestWeather_States(t *testing.T) {
	f := newFakeMeteo(t)
	clock := newFakeClock()
	w := newTestWeather(t, f, clock)
	w.Configure(WeatherSettings{Enabled: true, Place: "Rotterdam", Lat: fp(51.92), Lon: fp(4.48), Units: "c"})
	ctx := context.Background()

	require.NoError(t, w.RefreshNow(ctx))
	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
	clock.Advance(15 * time.Minute)
	assert.Error(t, w.RefreshNow(ctx))
	assert.Equal(t, StateOK, w.View().State, "a reading under 30 min old is still current")

	clock.Advance(15*time.Minute + time.Second)
	v := w.View()
	assert.Equal(t, StateStale, v.State, "no good fetch in 30 min: keep the reading, marked")
	assert.NotNil(t, v.Reading)

	clock.Advance(3 * time.Hour)
	v = w.View()
	assert.Equal(t, StateError, v.State)
	assert.Nil(t, v.Reading, "a reading this old isn't shown")

	f2 := newFakeMeteo(t)
	f2.fail = true
	w2 := newTestWeather(t, f2, newFakeClock())
	w2.Configure(WeatherSettings{Enabled: true, Place: "X", Lat: fp(1), Lon: fp(2), Units: "c"})
	assert.Error(t, w2.RefreshNow(ctx))
	assert.Equal(t, StateError, w2.View().State)
}

func TestConditionFor(t *testing.T) {
	cases := map[int]string{
		0: "clear", 1: "partly_cloudy", 2: "partly_cloudy", 3: "cloudy",
		45: "fog", 48: "fog",
		51: "rain", 55: "rain", 56: "rain", 61: "rain", 65: "rain", 67: "rain", 80: "rain", 82: "rain",
		71: "snow", 75: "snow", 77: "snow", 85: "snow", 86: "snow",
		95: "storm", 96: "storm", 99: "storm",
	}
	for code, want := range cases {
		assert.Equal(t, want, ConditionFor(code), "WMO %d", code)
	}
	assert.Equal(t, "cloudy", ConditionFor(42), "an unknown code reads as cloudy")
}

func TestWeather_Geocode(t *testing.T) {
	f := newFakeMeteo(t)
	w := newTestWeather(t, f, newFakeClock())
	places, err := w.Geocode(context.Background(), "Rotterdam")
	require.NoError(t, err)
	assert.Equal(t, "Rotterdam", f.query().Get("name"))
	assert.Equal(t, "5", f.query().Get("count"))
	require.Len(t, places, 2)
	assert.Equal(t, Place{Name: "Rotterdam", Region: "South Holland", Country: "Netherlands", Lat: 51.92, Lon: 4.48}, places[0])
	assert.Equal(t, -73.97, places[1].Lon, "coordinates are rounded to 2 decimals")

	places, err = w.Geocode(context.Background(), "Nowhereville")
	require.NoError(t, err)
	assert.NotNil(t, places)
	assert.Empty(t, places)

	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
	_, err = w.Geocode(context.Background(), "Rotterdam")
	assert.Error(t, err)
}

func TestRoundCoord(t *testing.T) {
	assert.Equal(t, 51.92, RoundCoord(51.92250))
	assert.Equal(t, 4.48, RoundCoord(4.47917))
	assert.Equal(t, -73.97, RoundCoord(-73.97069))
	assert.Equal(t, 0.0, RoundCoord(0.004))
}

func TestWeather_RunLoadsAndRefreshesEvery15Min(t *testing.T) {
	f := newFakeMeteo(t)
	clock := newFakeClock()
	w := NewWeather(WeatherOptions{
		Client: NewClient(), ForecastURL: f.srv.URL + "/v1/forecast", GeocodeURL: f.srv.URL + "/v1/search",
		Now: clock.Now, Tick: 5 * time.Millisecond,
		Load: func(context.Context) (WeatherSettings, error) {
			return WeatherSettings{Enabled: true, Place: "Rotterdam", Lat: fp(51.92), Lon: fp(4.48), Units: "c"}, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	require.Eventually(t, func() bool { return f.forecasts.Load() == 1 }, 2*time.Second, 5*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, int32(1), f.forecasts.Load(), "not again before 15 min")
	clock.Advance(WeatherInterval)
	require.Eventually(t, func() bool { return f.forecasts.Load() == 2 }, 2*time.Second, 5*time.Millisecond)
}
