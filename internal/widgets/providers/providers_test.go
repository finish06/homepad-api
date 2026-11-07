package providers

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/widgets"
)

// SPEC-status-widgets §6.5 — the Phase 1 providers, each against an httptest
// fake of its upstream API. No live calls.

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

// hits records the paths (with query) a fake upstream served.
type hits struct {
	mu    sync.Mutex
	paths []string
}

func (h *hits) add(r *http.Request) {
	h.mu.Lock()
	h.paths = append(h.paths, r.URL.Path)
	h.mu.Unlock()
}
func (h *hits) has(p string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, x := range h.paths {
		if x == p {
			return true
		}
	}
	return false
}

func num(t *testing.T, res widgets.Result, key string) float64 {
	t.Helper()
	v, ok := res.Values[key]
	require.True(t, ok, "field %q missing", key)
	require.NotNil(t, v.Num)
	return *v.Num
}

func TestDefaultRegistry(t *testing.T) {
	reg := Default()
	var kinds []string
	for _, p := range reg.All() {
		kinds = append(kinds, p.Kind())
		assert.NotEmpty(t, p.Label())
		assert.NotEmpty(t, p.Help())
		assert.NotEmpty(t, p.Match())
		assert.Equal(t, widgets.AuthAPIKey, p.Auth())
		assert.LessOrEqual(t, len(widgets.DefaultFields(p)), 3, "%s: at most 3 defaults", p.Kind())
		seen := map[string]bool{}
		for _, f := range p.Fields() {
			assert.False(t, seen[f.Key], "%s: duplicate field %s", p.Kind(), f.Key)
			seen[f.Key] = true
			assert.NotEmpty(t, f.Label)
		}
	}
	assert.Equal(t, []string{"jellyfin", "sonarr", "radarr"}, kinds)
}

// --- Jellyfin -----------------------------------------------------------

func fakeJellyfin(t *testing.T, prefix string, h *hits) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.add(r)
		path := strings.TrimPrefix(r.URL.Path, prefix)
		if path == "/System/Info/Public" {
			_, _ = w.Write([]byte(`{"Version":"10.10.3","ServerName":"media"}`))
			return
		}
		if r.Header.Get("Authorization") != `MediaBrowser Token="jf-key"` {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch path {
		case "/Sessions":
			if r.URL.Query().Get("activeWithinSeconds") != "960" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`[
				{"Id":"1","UserName":"a","NowPlayingItem":{"Name":"A Movie Title"}},
				{"Id":"2","UserName":"b"},
				{"Id":"3","UserName":"c","NowPlayingItem":{"Name":"Another"}},
				{"Id":"4","NowPlayingItem":null}
			]`))
		case "/Items/Counts":
			_, _ = w.Write([]byte(`{"MovieCount":1204,"SeriesCount":86,"EpisodeCount":4100,"SongCount":12000,"BookCount":3}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestJellyfin_Metadata(t *testing.T) {
	p := Jellyfin()
	assert.Equal(t, "jellyfin", p.Kind())
	assert.Equal(t, "Jellyfin", p.Label())
	var keys, labels []string
	for _, f := range p.Fields() {
		keys = append(keys, f.Key)
		labels = append(labels, f.Label)
		assert.Equal(t, widgets.UnitCount, f.Unit)
	}
	assert.Equal(t, []string{"playing", "movies", "series", "episodes", "songs", "books"}, keys)
	assert.Equal(t, []string{"playing", "movies", "shows", "episodes", "songs", "books"}, labels)
	assert.Equal(t, []string{"playing", "movies", "series"}, widgets.DefaultFields(p))
	assert.Contains(t, p.Match(), "jellyfin")
}

func TestJellyfin_FetchesEveryField(t *testing.T) {
	h := &hits{}
	srv := fakeJellyfin(t, "", h)
	res, err := Jellyfin().Fetch(context.Background(), widgets.NewClient(), widgets.Config{
		BaseURL: mustURL(t, srv.URL), Secret: "jf-key", WantVersion: true,
	}, &widgets.State{})
	require.NoError(t, err)
	assert.Equal(t, 2.0, num(t, res, "playing"), "sessions with a NowPlayingItem")
	assert.Equal(t, 1204.0, num(t, res, "movies"))
	assert.Equal(t, 86.0, num(t, res, "series"))
	assert.Equal(t, 4100.0, num(t, res, "episodes"))
	assert.Equal(t, 12000.0, num(t, res, "songs"))
	assert.Equal(t, 3.0, num(t, res, "books"))
	assert.Equal(t, "10.10.3", res.Version)
}

func TestJellyfin_OnlyTheEndpointsTheFieldsNeed(t *testing.T) {
	h := &hits{}
	srv := fakeJellyfin(t, "/jellyfin", h)
	res, err := Jellyfin().Fetch(context.Background(), widgets.NewClient(), widgets.Config{
		BaseURL: mustURL(t, srv.URL+"/jellyfin/"), Secret: "jf-key", Fields: []string{"playing"},
	}, &widgets.State{})
	require.NoError(t, err)
	assert.Equal(t, 2.0, num(t, res, "playing"))
	assert.True(t, h.has("/jellyfin/Sessions"), "the address's path prefix is kept")
	assert.False(t, h.has("/jellyfin/Items/Counts"))
	assert.False(t, h.has("/jellyfin/System/Info/Public"), "no version on a scheduled refresh")
	assert.Empty(t, res.Version)
}

func TestJellyfin_Errors(t *testing.T) {
	srv := fakeJellyfin(t, "", &hits{})
	_, err := Jellyfin().Fetch(context.Background(), widgets.NewClient(), widgets.Config{
		BaseURL: mustURL(t, srv.URL), Secret: "wrong",
	}, &widgets.State{})
	assert.ErrorIs(t, err, widgets.ErrAuth)

	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"MovieCount":"lots"}`))
	}))
	defer junk.Close()
	_, err = Jellyfin().Fetch(context.Background(), widgets.NewClient(), widgets.Config{
		BaseURL: mustURL(t, junk.URL), Secret: "k", Fields: []string{"movies"},
	}, &widgets.State{})
	assert.ErrorIs(t, err, widgets.ErrBadResponse)

	partial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"MovieCount":5}`))
	}))
	defer partial.Close()
	res, err := Jellyfin().Fetch(context.Background(), widgets.NewClient(), widgets.Config{
		BaseURL: mustURL(t, partial.URL), Secret: "k", Fields: []string{"movies", "books"},
	}, &widgets.State{})
	require.NoError(t, err)
	assert.Equal(t, 5.0, num(t, res, "movies"))
	_, ok := res.Values["books"]
	assert.False(t, ok, "a field the app didn't answer is absent")
}

// --- Sonarr / Radarr ----------------------------------------------------

func fakeArr(t *testing.T, h *hits, gotCal *url.Values) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.add(r)
		if r.Header.Get("X-Api-Key") != "arr-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v3/calendar":
			q := r.URL.Query()
			*gotCal = q
			_, _ = w.Write([]byte(`[{"id":1,"title":"Ep 1"},{"id":2,"title":"Ep 2"},{"id":3,"title":"Ep 3"}]`))
		case "/api/v3/queue/status":
			_, _ = w.Write([]byte(`{"totalCount":1,"count":1,"errors":false}`))
		case "/api/v3/wanted/missing":
			q := r.URL.Query()
			if q.Get("pageSize") != "1" || q.Get("monitored") != "true" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"page":1,"pageSize":1,"totalRecords":12,"records":[{"title":"x"}]}`))
		case "/api/v3/system/status":
			_, _ = w.Write([]byte(`{"version":"4.0.11.2680","appName":"Sonarr"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestArr_Metadata(t *testing.T) {
	for _, p := range []widgets.Provider{Sonarr(), Radarr()} {
		var keys, labels []string
		for _, f := range p.Fields() {
			keys = append(keys, f.Key)
			labels = append(labels, f.Label)
		}
		assert.Equal(t, []string{"upcoming", "queued", "missing"}, keys)
		assert.Equal(t, []string{"this week", "in queue", "missing"}, labels)
		assert.Equal(t, keys, widgets.DefaultFields(p))
	}
	assert.Equal(t, "sonarr", Sonarr().Kind())
	assert.Equal(t, "Sonarr", Sonarr().Label())
	assert.Equal(t, "radarr", Radarr().Kind())
	assert.Equal(t, "Radarr", Radarr().Label())
	assert.Contains(t, Sonarr().Match(), "sonarr")
	assert.Contains(t, Radarr().Match(), "radarr")
}

func TestArr_Fetch(t *testing.T) {
	now := time.Date(2026, 11, 7, 14, 2, 11, 0, time.UTC)
	for _, p := range []widgets.Provider{sonarrAt(func() time.Time { return now }), radarrAt(func() time.Time { return now })} {
		h := &hits{}
		var cal url.Values
		srv := fakeArr(t, h, &cal)
		res, err := p.Fetch(context.Background(), widgets.NewClient(), widgets.Config{
			BaseURL: mustURL(t, srv.URL), Secret: "arr-key", WantVersion: true,
		}, &widgets.State{})
		require.NoError(t, err, p.Kind())
		assert.Equal(t, 3.0, num(t, res, "upcoming"))
		assert.Equal(t, 1.0, num(t, res, "queued"))
		assert.Equal(t, 12.0, num(t, res, "missing"))
		assert.Equal(t, "4.0.11.2680", res.Version)
		assert.Equal(t, "2026-11-07T14:02:11Z", cal.Get("start"))
		assert.Equal(t, "2026-11-14T14:02:11Z", cal.Get("end"))
		assert.False(t, h.has("/api/v3/series"), "never the whole library")
		assert.False(t, h.has("/api/v3/movie"))
	}
}

func TestArr_OnlyNeededEndpointsAndErrors(t *testing.T) {
	h := &hits{}
	var cal url.Values
	srv := fakeArr(t, h, &cal)
	res, err := Sonarr().Fetch(context.Background(), widgets.NewClient(), widgets.Config{
		BaseURL: mustURL(t, srv.URL), Secret: "arr-key", Fields: []string{"queued"},
	}, &widgets.State{})
	require.NoError(t, err)
	assert.Equal(t, 1.0, num(t, res, "queued"))
	assert.False(t, h.has("/api/v3/calendar"))
	assert.False(t, h.has("/api/v3/wanted/missing"))
	assert.False(t, h.has("/api/v3/system/status"))

	_, err = Radarr().Fetch(context.Background(), widgets.NewClient(), widgets.Config{
		BaseURL: mustURL(t, srv.URL), Secret: "nope",
	}, &widgets.State{})
	assert.ErrorIs(t, err, widgets.ErrAuth)

	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"totalCount":"x"}`))
	}))
	defer junk.Close()
	_, err = Radarr().Fetch(context.Background(), widgets.NewClient(), widgets.Config{
		BaseURL: mustURL(t, junk.URL), Secret: "k", Fields: []string{"queued"},
	}, &widgets.State{})
	assert.ErrorIs(t, err, widgets.ErrBadResponse)
}

// Values are numbers only: no title, name or other upstream text survives
// into a Result (spec §6.5 "counts never include titles", AC-019).
func TestProviders_ResultsHoldNumbersOnly(t *testing.T) {
	h := &hits{}
	jf := fakeJellyfin(t, "", h)
	res, err := Jellyfin().Fetch(context.Background(), widgets.NewClient(), widgets.Config{BaseURL: mustURL(t, jf.URL), Secret: "jf-key"}, &widgets.State{})
	require.NoError(t, err)
	b, _ := json.Marshal(res)
	assert.NotContains(t, string(b), "A Movie Title")
	assert.NotContains(t, string(b), "media")
}

// The rule "every provider uses the guarded client" is enforced, not
// asserted: no provider source may build its own HTTP client or reach for
// net/http's defaults.
func TestProviders_UseOnlyTheGuardedClient(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	forbidden := map[string]bool{
		"http.Client": true, "http.Transport": true, "http.DefaultClient": true, "http.DefaultTransport": true,
		"http.Get": true, "http.Post": true, "http.Head": true, "http.PostForm": true, "http.NewRequest": true,
		"http.NewRequestWithContext": true, "net.Dial": true, "net.DialTimeout": true, "net.Dialer": true,
		"tls.Dial": true,
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, f, src, 0)
		require.NoError(t, err)
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && forbidden[x.Name+"."+sel.Sel.Name] {
				t.Errorf("%s: %s.%s — providers must use the guarded *widgets.Client only (AC-017)",
					fset.Position(sel.Pos()), x.Name, sel.Sel.Name)
			}
			return true
		})
	}
	require.GreaterOrEqual(t, checked, 2, "no provider sources were checked")
}
