package storage

import (
	"context"
	"time"
)

// RED stub — implemented in the GREEN commit.

type Widget struct {
	ServiceID        string
	Kind             string
	Address          string
	Username         string
	SecretCiphertext []byte
	SecretNonce      []byte
	Fields           []string
	IntervalS        int
	InsecureTLS      bool
	Enabled          bool
	ServiceURL       string
	UpdatedAt        time.Time
}

func (s *Store) Widget(ctx context.Context, serviceID string) (Widget, error) {
	return Widget{}, ErrNotFound
}
func (s *Store) PutWidget(ctx context.Context, w Widget) (Widget, error) {
	return Widget{}, ErrNotFound
}
func (s *Store) DeleteWidget(ctx context.Context, serviceID string) error { return nil }
func (s *Store) EnabledWidgets(ctx context.Context) ([]Widget, error)     { return nil, nil }

type WidgetSettings struct {
	ClockEnabled     bool
	ClockHour12      *bool
	WeatherEnabled   bool
	WeatherPlace     string
	WeatherLat       *float64
	WeatherLon       *float64
	WeatherUnits     string
	WebSearchEnabled bool
	WebSearchURL     string
	KidWebSearch     bool
}

func (s *Store) WidgetSettings(ctx context.Context) (WidgetSettings, error) {
	return WidgetSettings{}, nil
}
func (s *Store) UpsertWidgetSettings(ctx context.Context, ws WidgetSettings) (WidgetSettings, error) {
	return WidgetSettings{}, nil
}
func (s *Store) SetShowWidgets(ctx context.Context, userID string, show bool) error { return nil }
