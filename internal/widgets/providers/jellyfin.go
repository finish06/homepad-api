package providers

import (
	"context"
	"encoding/json"
	"net/url"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/widgets"
)

// Jellyfin — auth api_key, sent as Authorization: MediaBrowser Token="<key>".
// Key: Jellyfin Dashboard → API Keys.
type jellyfin struct{}

// Jellyfin returns the Jellyfin provider.
func Jellyfin() widgets.Provider { return jellyfin{} }

func (jellyfin) Kind() string           { return "jellyfin" }
func (jellyfin) Label() string          { return "Jellyfin" }
func (jellyfin) Auth() widgets.AuthType { return widgets.AuthAPIKey }
func (jellyfin) Match() []string        { return []string{"jellyfin"} }
func (jellyfin) Help() string           { return "In Jellyfin: Dashboard → API Keys → +." }

func (jellyfin) Fields() []widgets.FieldSpec {
	return []widgets.FieldSpec{
		{Key: "playing", Label: "playing", Unit: widgets.UnitCount, Default: true},
		{Key: "movies", Label: "movies", Unit: widgets.UnitCount, Default: true},
		{Key: "series", Label: "shows", Unit: widgets.UnitCount, Default: true},
		{Key: "episodes", Label: "episodes", Unit: widgets.UnitCount},
		{Key: "songs", Label: "songs", Unit: widgets.UnitCount},
		{Key: "books", Label: "books", Unit: widgets.UnitCount},
	}
}

// jellyfinSession keeps only whether something is playing: titles and user
// names are never decoded (spec §12).
type jellyfinSession struct {
	NowPlayingItem json.RawMessage `json:"NowPlayingItem"`
}

type jellyfinCounts struct {
	MovieCount   *float64 `json:"MovieCount"`
	SeriesCount  *float64 `json:"SeriesCount"`
	EpisodeCount *float64 `json:"EpisodeCount"`
	SongCount    *float64 `json:"SongCount"`
	BookCount    *float64 `json:"BookCount"`
}

func (jellyfin) Fetch(ctx context.Context, c *widgets.Client, cfg widgets.Config, _ *widgets.State) (widgets.Result, error) {
	h := header("Authorization", `MediaBrowser Token="`+cfg.Secret+`"`)
	res := widgets.Result{Values: map[string]widgets.Value{}}

	if cfg.Wants("playing") {
		var sessions []jellyfinSession
		q := url.Values{"activeWithinSeconds": {"960"}}
		if err := c.GetJSON(ctx, endpoint(cfg.BaseURL, "/Sessions", q), h, cfg.InsecureTLS, &sessions); err != nil {
			return widgets.Result{}, err
		}
		n := 0
		for _, s := range sessions {
			if len(s.NowPlayingItem) > 0 && string(s.NowPlayingItem) != "null" {
				n++
			}
		}
		res.Values["playing"] = count(float64(n))
	}

	if cfg.Wants("movies", "series", "episodes", "songs", "books") {
		var counts jellyfinCounts
		if err := c.GetJSON(ctx, endpoint(cfg.BaseURL, "/Items/Counts", nil), h, cfg.InsecureTLS, &counts); err != nil {
			return widgets.Result{}, err
		}
		for key, v := range map[string]*float64{
			"movies": counts.MovieCount, "series": counts.SeriesCount, "episodes": counts.EpisodeCount,
			"songs": counts.SongCount, "books": counts.BookCount,
		} {
			if v != nil {
				res.Values[key] = count(*v)
			}
		}
	}

	if cfg.WantVersion {
		var info struct {
			Version string `json:"Version"`
		}
		if err := c.GetJSON(ctx, endpoint(cfg.BaseURL, "/System/Info/Public", nil), nil, cfg.InsecureTLS, &info); err == nil {
			res.Version = info.Version
		}
	}
	return res, nil
}
