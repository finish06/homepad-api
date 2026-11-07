package widgets

import (
	"context"
	"time"
)

// RED stub — implemented in the GREEN commit.

const (
	DefaultForecastURL = ""
	DefaultGeocodeURL  = ""
	WeatherInterval    = 0 * time.Minute
)

type WeatherSettings struct {
	Enabled bool
	Place   string
	Lat     *float64
	Lon     *float64
	Units   string
}

type WeatherOptions struct {
	Client      *Client
	ForecastURL string
	GeocodeURL  string
	Now         func() time.Time
	Logf        func(format string, args ...any)
	Load        func(ctx context.Context) (WeatherSettings, error)
	Tick        time.Duration
}

type Reading struct {
	Temp, High, Low float64
	Condition       string
	IsDay           bool
}

type WeatherView struct {
	State   string
	AsOf    *time.Time
	Reading *Reading
}

type Place struct {
	Name    string  `json:"name"`
	Region  string  `json:"region"`
	Country string  `json:"country"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
}

type Weather struct{}

func NewWeather(o WeatherOptions) *Weather                                { return &Weather{} }
func (w *Weather) Run(ctx context.Context) error                          { return nil }
func (w *Weather) Configure(s WeatherSettings)                            {}
func (w *Weather) Settings() WeatherSettings                              { return WeatherSettings{} }
func (w *Weather) RefreshNow(ctx context.Context) error                   { return nil }
func (w *Weather) View() WeatherView                                      { return WeatherView{} }
func (w *Weather) Geocode(ctx context.Context, q string) ([]Place, error) { return nil, nil }

func ConditionFor(code int) string { return "" }
func RoundCoord(f float64) float64 { return f }
