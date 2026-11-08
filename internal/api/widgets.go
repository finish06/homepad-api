package api

import (
	"context"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/widgets"
)

// RED stub — implemented in the GREEN commit.

type noSource struct{}

func (noSource) EnabledWidgets(context.Context) ([]widgets.Stored, error) { return nil, nil }
func (noSource) Widget(context.Context, string) (widgets.Stored, bool, error) {
	return widgets.Stored{}, false, nil
}

func NewWidgetSource(store *storage.Store) widgets.Source { return noSource{} }

func LoadWeatherSettings(ctx context.Context, store *storage.Store) (widgets.WeatherSettings, error) {
	return widgets.WeatherSettings{}, nil
}
