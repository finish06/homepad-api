package providers

import (
	"context"
	"encoding/json"
	"net/url"
	"time"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/widgets"
)

// Sonarr and Radarr share one API (v3), so one provider serves both kinds.
// Auth api_key, header X-Api-Key. Key: Settings → General → API Key.
//
// /api/v3/series and /api/v3/movie are deliberately not used: they return the
// whole library, which can run to several MB. Every endpoint here returns a
// count or a summary.
type arr struct {
	kind, label string
	now         func() time.Time
}

// Sonarr returns the Sonarr provider.
func Sonarr() widgets.Provider { return sonarrAt(time.Now) }

// Radarr returns the Radarr provider.
func Radarr() widgets.Provider { return radarrAt(time.Now) }

func sonarrAt(now func() time.Time) widgets.Provider {
	return arr{kind: "sonarr", label: "Sonarr", now: now}
}

func radarrAt(now func() time.Time) widgets.Provider {
	return arr{kind: "radarr", label: "Radarr", now: now}
}

func (a arr) Kind() string         { return a.kind }
func (a arr) Label() string        { return a.label }
func (arr) Auth() widgets.AuthType { return widgets.AuthAPIKey }
func (a arr) Match() []string      { return []string{a.kind} }
func (a arr) Help() string         { return "In " + a.label + ": Settings → General → API Key." }

func (arr) Fields() []widgets.FieldSpec {
	return []widgets.FieldSpec{
		{Key: "upcoming", Label: "this week", Unit: widgets.UnitCount, Default: true},
		{Key: "queued", Label: "in queue", Unit: widgets.UnitCount, Default: true},
		{Key: "missing", Label: "missing", Unit: widgets.UnitCount, Default: true},
	}
}

func (a arr) Fetch(ctx context.Context, c *widgets.Client, cfg widgets.Config, _ *widgets.State) (widgets.Result, error) {
	h := header("X-Api-Key", cfg.Secret)
	res := widgets.Result{Values: map[string]widgets.Value{}}

	if cfg.Wants("upcoming") {
		now := a.now().UTC().Truncate(time.Second)
		q := url.Values{
			"start": {now.Format(time.RFC3339)},
			"end":   {now.Add(7 * 24 * time.Hour).Format(time.RFC3339)},
		}
		// Only the array's length is used; items are never decoded.
		var items []json.RawMessage
		if err := c.GetJSON(ctx, endpoint(cfg.BaseURL, "/api/v3/calendar", q), h, cfg.InsecureTLS, &items); err != nil {
			return widgets.Result{}, err
		}
		res.Values["upcoming"] = count(float64(len(items)))
	}

	if cfg.Wants("queued") {
		var st struct {
			TotalCount *float64 `json:"totalCount"`
		}
		if err := c.GetJSON(ctx, endpoint(cfg.BaseURL, "/api/v3/queue/status", nil), h, cfg.InsecureTLS, &st); err != nil {
			return widgets.Result{}, err
		}
		if st.TotalCount != nil {
			res.Values["queued"] = count(*st.TotalCount)
		}
	}

	if cfg.Wants("missing") {
		var page struct {
			TotalRecords *float64 `json:"totalRecords"`
		}
		q := url.Values{"pageSize": {"1"}, "monitored": {"true"}}
		if err := c.GetJSON(ctx, endpoint(cfg.BaseURL, "/api/v3/wanted/missing", q), h, cfg.InsecureTLS, &page); err != nil {
			return widgets.Result{}, err
		}
		if page.TotalRecords != nil {
			res.Values["missing"] = count(*page.TotalRecords)
		}
	}

	if cfg.WantVersion {
		var st struct {
			Version string `json:"version"`
		}
		if err := c.GetJSON(ctx, endpoint(cfg.BaseURL, "/api/v3/system/status", nil), h, cfg.InsecureTLS, &st); err == nil {
			res.Version = st.Version
		}
	}
	return res, nil
}
