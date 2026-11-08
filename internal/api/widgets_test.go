package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-status-widgets §7 — the widget API, against httptest fakes of the
// upstream apps. No live calls.

const arrKey = "arr-key-SECRET-7f3a9c41"

// fakeArrApp is a Sonarr/Radarr API that answers only with arrKey.
type fakeArrApp struct {
	*httptest.Server
	hits     atomic.Int32
	redirect atomic.Bool
	deny     atomic.Bool
}

func newFakeArrApp(t *testing.T) *fakeArrApp {
	t.Helper()
	f := &fakeArrApp{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.redirect.Load() {
			http.Redirect(w, r, "/sso/login", http.StatusFound)
			return
		}
		if f.deny.Load() || r.Header.Get("X-Api-Key") != arrKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Unauthorized upstream text"}`))
			return
		}
		switch r.URL.Path {
		case "/api/v3/calendar":
			_, _ = w.Write([]byte(`[{"title":"Ep A"},{"title":"Ep B"},{"title":"Ep C"}]`))
		case "/api/v3/queue/status":
			_, _ = w.Write([]byte(`{"totalCount":1}`))
		case "/api/v3/wanted/missing":
			_, _ = w.Write([]byte(`{"totalRecords":12}`))
		case "/api/v3/system/status":
			_, _ = w.Write([]byte(`{"version":"4.0.11"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// calls records every response body a test receives, so a leak scan can
// cover all of them.
type calls struct {
	t      *testing.T
	base   string
	bodies [][]byte
}

func (c *calls) do(method, path, token string, body any) (int, []byte) {
	c.t.Helper()
	resp := doJSON(c.t, method, c.base+path, token, body)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	c.bodies = append(c.bodies, b)
	return resp.StatusCode, b
}

func decodeMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m), string(b))
	return m
}

func widgetBody(address string, extra map[string]any) map[string]any {
	b := map[string]any{
		"kind": "sonarr", "address": address, "fields": []string{"missing", "upcoming"},
		"intervalS": 60, "insecureTls": false, "enabled": true,
	}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// serviceWidget returns the "widget" object of one service in a services
// body, and whether the key was present at all.
func serviceWidget(t *testing.T, body []byte, id string) (map[string]any, bool) {
	t.Helper()
	var payload struct {
		Services []map[string]json.RawMessage `json:"services"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	for _, s := range payload.Services {
		var sid string
		_ = json.Unmarshal(s["id"], &sid)
		if sid != id {
			continue
		}
		raw, ok := s["widget"]
		if !ok {
			return nil, false
		}
		var w map[string]any
		require.NoError(t, json.Unmarshal(raw, &w))
		return w, true
	}
	t.Fatalf("service %s not in the list", id)
	return nil, false
}

func TestWidgets_Kinds(t *testing.T) {
	env := testsupport.NewEnv(t)
	c := &calls{t: t, base: env.URL}
	code, b := c.do(http.MethodGet, "/api/admin/widgets/kinds", "admin-session", nil)
	require.Equal(t, http.StatusOK, code)
	var out struct {
		SecretKeySet bool `json:"secretKeySet"`
		Kinds        []struct {
			Kind   string   `json:"kind"`
			Label  string   `json:"label"`
			Auth   string   `json:"auth"`
			Match  []string `json:"match"`
			Help   string   `json:"help"`
			Fields []struct {
				Key     string `json:"key"`
				Label   string `json:"label"`
				Unit    string `json:"unit"`
				Default bool   `json:"default"`
			} `json:"fields"`
		} `json:"kinds"`
	}
	require.NoError(t, json.Unmarshal(b, &out))
	assert.True(t, out.SecretKeySet)
	require.Len(t, out.Kinds, 3)
	assert.Equal(t, "jellyfin", out.Kinds[0].Kind)
	assert.Equal(t, "Jellyfin", out.Kinds[0].Label)
	assert.Equal(t, "api_key", out.Kinds[0].Auth)
	assert.Contains(t, out.Kinds[0].Match, "jellyfin")
	assert.NotEmpty(t, out.Kinds[0].Help)
	assert.Equal(t, "playing", out.Kinds[0].Fields[0].Key)
	assert.True(t, out.Kinds[0].Fields[0].Default)
	assert.Equal(t, "count", out.Kinds[0].Fields[0].Unit)
	assert.Equal(t, []string{"sonarr", "radarr"}, []string{out.Kinds[1].Kind, out.Kinds[2].Kind})
	assert.Equal(t, "this week", out.Kinds[1].Fields[0].Label)

	noKey := testsupport.NewEnvWidgets(t, nil, testsupport.WidgetOptions{NoSecretKey: true})
	code, b = (&calls{t: t, base: noKey.URL}).do(http.MethodGet, "/api/admin/widgets/kinds", "admin-session", nil)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, false, decodeMap(t, b)["secretKeySet"], "the admin can tell the server key is unset")

	code, _ = c.do(http.MethodGet, "/api/admin/widgets/kinds", "non-admin-session", nil)
	assert.Equal(t, http.StatusForbidden, code)
}

func TestWidgets_PutGetDelete(t *testing.T) {
	env := testsupport.NewEnv(t)
	app := newFakeArrApp(t)
	id := env.SeedService(t, "sonarr", "Sonarr", "")
	c := &calls{t: t, base: env.URL}

	code, _ := c.do(http.MethodGet, "/api/services/"+id+"/widget", "admin-session", nil)
	assert.Equal(t, http.StatusNotFound, code, "no widget yet")

	code, b := c.do(http.MethodPut, "/api/services/"+id+"/widget", "admin-session",
		widgetBody(app.URL, map[string]any{"secret": arrKey, "intervalS": 300}))
	require.Equal(t, http.StatusOK, code, string(b))
	got := decodeMap(t, b)
	assert.Equal(t, "sonarr", got["kind"])
	assert.Equal(t, app.URL, got["address"])
	assert.Equal(t, "", got["username"])
	assert.Equal(t, true, got["hasSecret"])
	assert.Equal(t, []any{"upcoming", "missing"}, got["fields"], "stored in the provider's order (W-2)")
	assert.Equal(t, 300.0, got["intervalS"])
	assert.Equal(t, false, got["insecureTls"])
	assert.Equal(t, true, got["enabled"])
	assert.Equal(t, "loading", got["state"])
	assert.Equal(t, "", got["error"])
	assert.Nil(t, got["lastOkAt"])
	_, hasSecretField := got["secret"]
	assert.False(t, hasSecretField, "no response field can carry the key (AC-003)")

	code, b = c.do(http.MethodGet, "/api/services/"+id+"/widget", "admin-session", nil)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, decodeMap(t, b)["hasSecret"])

	// The manager fetched it on Reload's wake-up; drive it here.
	env.Widgets.RefreshNow(context.Background(), id)
	code, b = c.do(http.MethodGet, "/api/services/"+id+"/widget", "admin-session", nil)
	require.Equal(t, http.StatusOK, code)
	got = decodeMap(t, b)
	assert.Equal(t, "ok", got["state"])
	assert.NotNil(t, got["lastOkAt"])
	assert.NotNil(t, got["lastCheckAt"])

	code, _ = c.do(http.MethodDelete, "/api/services/"+id+"/widget", "admin-session", nil)
	assert.Equal(t, http.StatusNoContent, code)
	code, _ = c.do(http.MethodDelete, "/api/services/"+id+"/widget", "admin-session", nil)
	assert.Equal(t, http.StatusNoContent, code, "idempotent")
	code, _ = c.do(http.MethodGet, "/api/services/"+id+"/widget", "admin-session", nil)
	assert.Equal(t, http.StatusNotFound, code)
	code, b = c.do(http.MethodGet, "/api/services", "admin-session", nil)
	require.Equal(t, http.StatusOK, code)
	_, has := serviceWidget(t, b, id)
	assert.False(t, has, "a removed widget leaves the tile exactly as before")

	code, _ = c.do(http.MethodPut, "/api/services/00000000-0000-0000-0000-000000000000/widget", "admin-session", widgetBody(app.URL, nil))
	assert.Equal(t, http.StatusNotFound, code)
	code, _ = c.do(http.MethodGet, "/api/services/not-a-uuid/widget", "admin-session", nil)
	assert.Equal(t, http.StatusNotFound, code)
}

func TestWidgets_PutValidation422(t *testing.T) {
	env := testsupport.NewEnv(t)
	id := env.SeedService(t, "sonarr", "Sonarr", "")
	c := &calls{t: t, base: env.URL}
	cases := map[string]struct {
		body  map[string]any
		field string
	}{
		"unknown kind":      {widgetBody("", map[string]any{"kind": "plex"}), "kind"},
		"four fields":       {widgetBody("", map[string]any{"kind": "jellyfin", "fields": []string{"playing", "movies", "series", "books"}}), "fields"},
		"unknown field":     {widgetBody("", map[string]any{"fields": []string{"playing"}}), "fields"},
		"interval 120":      {widgetBody("", map[string]any{"intervalS": 120}), "intervalS"},
		"ftp address":       {widgetBody("ftp://sonarr.home.arpa/", nil), "address"},
		"relative address":  {widgetBody("sonarr.home.arpa:8989", nil), "address"},
		"address with user": {widgetBody("http://admin:pw@sonarr.home.arpa:8989", nil), "address"},
	}
	for name, tc := range cases {
		code, b := c.do(http.MethodPut, "/api/services/"+id+"/widget", "admin-session", tc.body)
		require.Equal(t, http.StatusUnprocessableEntity, code, name)
		m := decodeMap(t, b)
		assert.Equal(t, tc.field, m["field"], name)
		assert.NotEmpty(t, m["message"], name)
	}
	code, _ := c.do(http.MethodGet, "/api/services/"+id+"/widget", "admin-session", nil)
	assert.Equal(t, http.StatusNotFound, code, "a refused PUT saved nothing")

	// AC-004 — without HOMEPAD_SECRET_KEY the API refuses to store a key, and
	// a widget without one still saves.
	noKey := testsupport.NewEnvWidgets(t, nil, testsupport.WidgetOptions{NoSecretKey: true})
	nid := noKey.SeedService(t, "sonarr", "Sonarr", "")
	nc := &calls{t: t, base: noKey.URL}
	code, b := nc.do(http.MethodPut, "/api/services/"+nid+"/widget", "admin-session", widgetBody("", map[string]any{"secret": arrKey}))
	require.Equal(t, http.StatusUnprocessableEntity, code)
	assert.Equal(t, "secret", decodeMap(t, b)["field"])
	code, _ = nc.do(http.MethodPut, "/api/services/"+nid+"/widget", "admin-session", widgetBody("", nil))
	assert.Equal(t, http.StatusOK, code)
	code, _ = nc.do(http.MethodPut, "/api/services/"+nid+"/widget", "admin-session", widgetBody("", map[string]any{"secret": ""}))
	assert.Equal(t, http.StatusOK, code, `clearing ("") needs no server key`)
}

func TestWidgets_SecretIsWriteOnlyAndBoundToOrigin(t *testing.T) {
	env := testsupport.NewEnv(t)
	app := newFakeArrApp(t)
	id := env.SeedService(t, "sonarr", "Sonarr", "")
	c := &calls{t: t, base: env.URL}
	put := func(body map[string]any) map[string]any {
		t.Helper()
		code, b := c.do(http.MethodPut, "/api/services/"+id+"/widget", "admin-session", body)
		require.Equal(t, http.StatusOK, code, string(b))
		return decodeMap(t, b)
	}

	assert.Equal(t, true, put(widgetBody(app.URL, map[string]any{"secret": arrKey}))["hasSecret"])
	assert.Equal(t, true, put(widgetBody(app.URL, nil))["hasSecret"], "omitted = keep")
	assert.Equal(t, true, put(widgetBody(app.URL+"/sub/path", nil))["hasSecret"], "same origin, new path: kept")
	assert.Equal(t, false, put(widgetBody(app.URL, map[string]any{"secret": ""}))["hasSecret"], `"" = clear`)

	put(widgetBody(app.URL, map[string]any{"secret": arrKey}))
	assert.Equal(t, false, put(widgetBody(app.URL, map[string]any{"kind": "radarr"}))["hasSecret"],
		"a changed kind clears the key (AC-023)")

	put(widgetBody(app.URL, map[string]any{"kind": "radarr", "secret": arrKey}))
	assert.Equal(t, false, put(widgetBody("http://other.home.arpa:7878", map[string]any{"kind": "radarr"}))["hasSecret"],
		"a changed origin clears the key (AC-023)")

	// A new key sent WITH the change is kept: it was entered for the new host.
	assert.Equal(t, true, put(widgetBody(app.URL, map[string]any{"kind": "sonarr", "secret": arrKey}))["hasSecret"])

	// The fetch sends the key and works.
	env.Widgets.RefreshNow(context.Background(), id)
	_, b := c.do(http.MethodGet, "/api/services", "admin-session", nil)
	w, ok := serviceWidget(t, b, id)
	require.True(t, ok)
	assert.Equal(t, "ok", w["state"])
}

func TestWidgets_OnServicesForAdults(t *testing.T) {
	env := testsupport.NewEnv(t)
	app := newFakeArrApp(t)
	id := env.SeedService(t, "sonarr", "Sonarr", "")
	plain := env.SeedService(t, "plain", "Plain", "")
	c := &calls{t: t, base: env.URL}
	code, _ := c.do(http.MethodPut, "/api/services/"+id+"/widget", "admin-session", widgetBody(app.URL, map[string]any{"secret": arrKey}))
	require.Equal(t, http.StatusOK, code)

	// loading: the key is there with no fields and no code.
	_, b := c.do(http.MethodGet, "/api/services", "admin-session", nil)
	w, ok := serviceWidget(t, b, id)
	require.True(t, ok)
	assert.Equal(t, "loading", w["state"])
	assert.Equal(t, []any{}, w["fields"])

	env.Widgets.RefreshNow(context.Background(), id)
	for _, who := range []string{"admin-session", "non-admin-session"} {
		code, b := c.do(http.MethodGet, "/api/services", who, nil)
		require.Equal(t, http.StatusOK, code)
		w, ok := serviceWidget(t, b, id)
		require.True(t, ok, who)
		assert.Equal(t, "sonarr", w["kind"])
		assert.Equal(t, "ok", w["state"])
		asOf, _ := w["asOf"].(string)
		_, err := time.Parse(time.RFC3339, asOf)
		assert.NoError(t, err)
		assert.Equal(t, []any{
			map[string]any{"key": "upcoming", "label": "this week", "unit": "count", "value": 3.0},
			map[string]any{"key": "missing", "label": "missing", "unit": "count", "value": 12.0},
		}, w["fields"])
		assert.Len(t, w, 4, "kind, state, asOf, fields — nothing else (§7.2)")
		_, has := serviceWidget(t, b, plain)
		assert.False(t, has, "a tile without a widget has no widget key (AC-006)")
		assert.NotContains(t, string(b), app.URL, "no widget address in /api/services (AC-002)")
		assert.NotContains(t, string(b), strings.TrimPrefix(app.URL, "http://"))
		assert.NotContains(t, string(b), "Ep A", "titles never leave the server (§12)")
	}

	// Disabled → absent.
	code, _ = c.do(http.MethodPut, "/api/services/"+id+"/widget", "admin-session", widgetBody(app.URL, map[string]any{"enabled": false}))
	require.Equal(t, http.StatusOK, code)
	_, b = c.do(http.MethodGet, "/api/services", "admin-session", nil)
	_, has := serviceWidget(t, b, id)
	assert.False(t, has, "a disabled widget is absent")
}

// AC-013 — a broken widget never shows its error on the dashboard; the admin
// editor gets the code.
func TestWidgets_ErrorsStayInTheEditor(t *testing.T) {
	env := testsupport.NewEnv(t)
	app := newFakeArrApp(t)
	id := env.SeedService(t, "sonarr", "Sonarr", "")
	c := &calls{t: t, base: env.URL}
	code, _ := c.do(http.MethodPut, "/api/services/"+id+"/widget", "admin-session", widgetBody(app.URL, map[string]any{"secret": "revoked-key"}))
	require.Equal(t, http.StatusOK, code)
	env.Widgets.RefreshNow(context.Background(), id)

	_, b := c.do(http.MethodGet, "/api/services", "non-admin-session", nil)
	w, ok := serviceWidget(t, b, id)
	require.True(t, ok)
	assert.Equal(t, "error", w["state"])
	assert.Equal(t, []any{}, w["fields"])
	for _, leak := range []string{`"auth"`, "Unauthorized", "upstream text", `"error":"`} {
		assert.NotContains(t, string(b), leak)
	}

	_, b = c.do(http.MethodGet, "/api/services/"+id+"/widget", "admin-session", nil)
	got := decodeMap(t, b)
	assert.Equal(t, "error", got["state"])
	assert.Equal(t, "auth", got["error"])
	assert.NotContains(t, string(b), "upstream text")
}

// AC-005 / TC-010 — the server key changed: the widget is error /
// secret_unreadable, the editor can tell, and nothing else fails.
func TestWidgets_ServerKeyChanged(t *testing.T) {
	env := testsupport.NewEnv(t)
	app := newFakeArrApp(t)
	id := env.SeedService(t, "sonarr", "Sonarr", "")
	c := &calls{t: t, base: env.URL}
	code, _ := c.do(http.MethodPut, "/api/services/"+id+"/widget", "admin-session", widgetBody(app.URL, map[string]any{"secret": arrKey}))
	require.Equal(t, http.StatusOK, code)

	restarted := env.Restart(t, testsupport.WidgetOptions{SecretKey: strings.Repeat("ab", 32)})
	hitsBefore := app.hits.Load()
	restarted.Widgets.RefreshNow(context.Background(), id)
	assert.Equal(t, hitsBefore, app.hits.Load(), "the app is never called with an unreadable key")

	rc := &calls{t: t, base: restarted.URL}
	code, b := rc.do(http.MethodGet, "/api/services/"+id+"/widget", "admin-session", nil)
	require.Equal(t, http.StatusOK, code)
	got := decodeMap(t, b)
	assert.Equal(t, "error", got["state"])
	assert.Equal(t, "secret_unreadable", got["error"])
	assert.Equal(t, true, got["hasSecret"])

	for _, path := range []string{"/api/services", "/api/categories", "/api/me", "/api/widgets/info", "/api/admin/widgets/kinds"} {
		code, _ := rc.do(http.MethodGet, path, "admin-session", nil)
		assert.Equal(t, http.StatusOK, code, path)
	}

	// Entering the key again fixes it.
	code, _ = rc.do(http.MethodPut, "/api/services/"+id+"/widget", "admin-session", widgetBody(app.URL, map[string]any{"secret": arrKey}))
	require.Equal(t, http.StatusOK, code)
	restarted.Widgets.RefreshNow(context.Background(), id)
	_, b = rc.do(http.MethodGet, "/api/services/"+id+"/widget", "admin-session", nil)
	assert.Equal(t, "ok", decodeMap(t, b)["state"])
}

func TestWidgets_TestConnection(t *testing.T) {
	env := testsupport.NewEnv(t)
	app := newFakeArrApp(t)
	id := env.SeedService(t, "sonarr", "Sonarr", "")
	c := &calls{t: t, base: env.URL}
	test := func(body map[string]any) (int, map[string]any) {
		t.Helper()
		code, b := c.do(http.MethodPost, "/api/services/"+id+"/widget/test", "admin-session", body)
		if code != http.StatusOK {
			return code, nil
		}
		return code, decodeMap(t, b)
	}
	wait := func() { time.Sleep(2100 * time.Millisecond) }

	// Unsaved values, typed key: works before anything is saved (AC-021).
	code, got := test(widgetBody(app.URL, map[string]any{"secret": arrKey}))
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, got["ok"])
	assert.Equal(t, "4.0.11", got["version"])
	fields, _ := got["fields"].([]any)
	require.Len(t, fields, 2)
	assert.Equal(t, "upcoming", fields[0].(map[string]any)["key"])

	// 429 within 2s for the same admin.
	code, _ = test(widgetBody(app.URL, map[string]any{"secret": arrKey}))
	assert.Equal(t, http.StatusTooManyRequests, code)

	wait()
	code, got = test(widgetBody(app.URL, map[string]any{"secret": "wrong"}))
	require.Equal(t, http.StatusOK, code, "a failed test is a successful API call")
	assert.Equal(t, false, got["ok"])
	assert.Equal(t, "auth", got["error"])
	assert.Len(t, got, 2, "ok + code, never a body or a message")

	// Saved key: a blank key field means "use the saved key".
	code, _ = c.do(http.MethodPut, "/api/services/"+id+"/widget", "admin-session", widgetBody(app.URL, map[string]any{"secret": arrKey}))
	require.Equal(t, http.StatusOK, code)
	wait()
	_, got = test(widgetBody(app.URL, nil))
	assert.Equal(t, true, got["ok"], "omitted secret uses the saved one")

	// …unless the origin changed: the saved key isn't sent to another host.
	other := newFakeArrApp(t)
	wait()
	_, got = test(widgetBody(other.URL, nil))
	assert.Equal(t, false, got["ok"])
	assert.Equal(t, "auth", got["error"], "no key was sent to the new origin")

	// TC-005 — an SSO redirect.
	app.redirect.Store(true)
	wait()
	_, got = test(widgetBody(app.URL, nil))
	assert.Equal(t, "redirect", got["error"])
	app.redirect.Store(false)

	// TC-006 — a metadata address.
	wait()
	_, got = test(widgetBody("http://169.254.169.254/", map[string]any{"secret": "x"}))
	assert.Equal(t, "blocked_address", got["error"])

	// A test never changes what's saved.
	_, b := c.do(http.MethodGet, "/api/services/"+id+"/widget", "admin-session", nil)
	assert.Equal(t, app.URL, decodeMap(t, b)["address"])

	wait()
	code, _ = test(widgetBody("", map[string]any{"kind": "plex"}))
	assert.Equal(t, http.StatusUnprocessableEntity, code)

	code, _ = c.do(http.MethodPost, "/api/services/"+id+"/widget/test", "non-admin-session", widgetBody(app.URL, nil))
	assert.Equal(t, http.StatusForbidden, code)
}

// AC-002, AC-003, AC-019, §8.1 — a key written through PUT never appears in
// any response (admin responses included), in the logs, or as plaintext in
// the row.
func TestWidgets_KeyNeverLeaves(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	env := testsupport.NewEnv(t)
	app := newFakeArrApp(t)
	id := env.SeedService(t, "sonarr", "Sonarr", "")
	c := &calls{t: t, base: env.URL}

	c.do(http.MethodPut, "/api/services/"+id+"/widget", "admin-session", widgetBody(app.URL, map[string]any{"secret": arrKey}))
	c.do(http.MethodGet, "/api/services/"+id+"/widget", "admin-session", nil)
	env.Widgets.RefreshNow(context.Background(), id)
	for _, who := range []string{"admin-session", "non-admin-session"} {
		c.do(http.MethodGet, "/api/services", who, nil)
		c.do(http.MethodGet, "/api/widgets/info", who, nil)
		c.do(http.MethodGet, "/api/me", who, nil)
	}
	c.do(http.MethodGet, "/api/admin/widgets/kinds", "admin-session", nil)
	c.do(http.MethodPost, "/api/services/"+id+"/widget/test", "admin-session", widgetBody(app.URL, nil))
	c.do(http.MethodPut, "/api/services/"+id+"/widget", "admin-session", widgetBody(app.URL, map[string]any{"fields": []string{"queued"}}))
	// A failing fetch, so the manager logs.
	app.deny.Store(true)
	env.Widgets.RefreshNow(context.Background(), id)
	c.do(http.MethodGet, "/api/services/"+id+"/widget", "admin-session", nil)

	require.NotEmpty(t, c.bodies)
	for i, b := range c.bodies {
		assert.NotContains(t, string(b), arrKey, "response %d carries the key", i)
	}
	assert.NotContains(t, logs.String(), arrKey, "the key is never logged")
	assert.NotContains(t, logs.String(), strings.TrimPrefix(app.URL, "http://"), "logs never name the address")
	assert.Contains(t, logs.String(), id, "the failing widget is logged by id")

	conn, err := pgx.Connect(context.Background(), env.DSN)
	require.NoError(t, err)
	defer conn.Close(context.Background())
	var row, ctHex, ctB64, ctEsc string
	var nonceLen int
	require.NoError(t, conn.QueryRow(context.Background(),
		`SELECT row_to_json(w)::text, encode(secret_ciphertext, 'hex'), encode(secret_ciphertext, 'base64'),
		        encode(secret_ciphertext, 'escape'), length(secret_nonce)
		   FROM service_widgets w WHERE service_id = $1`, id).Scan(&row, &ctHex, &ctB64, &ctEsc, &nonceLen))
	for _, enc := range []string{arrKey, hex.EncodeToString([]byte(arrKey)), base64.StdEncoding.EncodeToString([]byte(arrKey))} {
		for _, hay := range []string{row, ctHex, ctB64, ctEsc} {
			assert.NotContains(t, hay, enc, "the row holds no plaintext key in any encoding")
		}
	}
	assert.Equal(t, 12, nonceLen, "a 12-byte nonce (AC-004)")
}

// AC-014 / TC-007 — a kid's services carry no widget key, even on an app the
// kid is allowed that has a working widget.
func TestWidgets_KidsSeeNoWidgetData(t *testing.T) {
	w := newKidWorld(t)
	app := newFakeArrApp(t)
	c := &calls{t: t, base: w.env.URL}
	for _, id := range []string{w.allowedID, w.hiddenID} {
		code, _ := c.do(http.MethodPut, "/api/services/"+id+"/widget", "admin-session", widgetBody(app.URL, map[string]any{"secret": arrKey}))
		require.Equal(t, http.StatusOK, code)
		w.env.Widgets.RefreshNow(context.Background(), id)
	}
	_, adult := c.do(http.MethodGet, "/api/services", "admin-session", nil)
	_, ok := serviceWidget(t, adult, w.allowedID)
	require.True(t, ok, "precondition: an adult sees the widget")

	code, b := c.do(http.MethodGet, "/api/services", w.kidToken, nil)
	require.Equal(t, http.StatusOK, code)
	assert.NotContains(t, string(b), `"widget"`)
	assert.NotContains(t, string(b), "this week")
	assertNoHiddenMarkers(t, w, "kid services", b)

	code, b = c.do(http.MethodGet, "/api/widgets/info", w.kidToken, nil)
	assert.Equal(t, http.StatusOK, code, "the strip is catalog-free: kids read it (AC-029)")
	assertNoHiddenMarkers(t, w, "kid widgets info", b)
}

func TestWidgets_Info(t *testing.T) {
	meteo := newFakeOpenMeteo(t)
	env := testsupport.NewEnvWidgets(t, nil, testsupport.WidgetOptions{ForecastURL: meteo.URL + "/v1/forecast", GeocodeURL: meteo.URL + "/v1/search"})
	c := &calls{t: t, base: env.URL}

	code, _ := (&calls{t: t, base: env.URL}).do(http.MethodGet, "/api/widgets/info", "", nil)
	assert.Equal(t, http.StatusUnauthorized, code, "never public (AC-029)")

	// AC-035 — everything starts off.
	code, b := c.do(http.MethodGet, "/api/widgets/info", "non-admin-session", nil)
	require.Equal(t, http.StatusOK, code)
	info := decodeMap(t, b)
	assert.Equal(t, map[string]any{"enabled": false, "hour12": nil}, info["clock"])
	weather := info["weather"].(map[string]any)
	assert.Equal(t, false, weather["enabled"])
	assert.Equal(t, "c", weather["units"])
	search := info["search"].(map[string]any)
	assert.Equal(t, false, search["enabled"])
	assert.Equal(t, "DuckDuckGo", search["name"])
	assert.Equal(t, "https://duckduckgo.com/?q={query}", search["url"])
	_, hasKids := search["kids"]
	assert.False(t, hasKids, "only an admin sees the kid switch")

	code, b = c.do(http.MethodPatch, "/api/admin/settings", "admin-session", map[string]any{
		"clockEnabled": true, "clockHour12": false,
		"weatherEnabled": true, "weatherPlace": "Rotterdam, Netherlands", "weatherLat": 51.9225, "weatherLon": 4.47917, "weatherUnits": "c",
		"webSearchEnabled": true, "webSearchUrl": "https://www.startpage.com/do/search?q={query}",
	})
	require.Equal(t, http.StatusOK, code, string(b))
	require.NoError(t, env.Weather.RefreshNow(context.Background()))
	assert.Equal(t, "51.92", meteo.lastQuery().Get("latitude"), "coordinates rounded to 2 decimals")

	code, b = c.do(http.MethodGet, "/api/widgets/info", "non-admin-session", nil)
	require.Equal(t, http.StatusOK, code)
	info = decodeMap(t, b)
	assert.Equal(t, map[string]any{"enabled": true, "hour12": false}, info["clock"])
	weather = info["weather"].(map[string]any)
	assert.Equal(t, true, weather["enabled"])
	assert.Equal(t, "ok", weather["state"])
	assert.Equal(t, "Rotterdam, Netherlands", weather["place"])
	assert.Equal(t, 7.7, weather["temp"])
	assert.Equal(t, 9.0, weather["high"])
	assert.Equal(t, 2.8, weather["low"])
	assert.Equal(t, "cloudy", weather["condition"])
	assert.Equal(t, false, weather["isDay"])
	assert.NotContains(t, string(b), "51.9", "stored coordinates are never returned")
	assert.NotContains(t, string(b), "4.48")
	search = info["search"].(map[string]any)
	assert.Equal(t, map[string]any{"enabled": true, "name": "Startpage", "url": "https://www.startpage.com/do/search?q={query}"}, search)

	// Kids: web search follows the kid switch (AC-030).
	_, kidTok := env.SeedKid(t, "Ava")
	_, b = c.do(http.MethodGet, "/api/widgets/info", kidTok, nil)
	assert.Equal(t, false, decodeMap(t, b)["search"].(map[string]any)["enabled"], "off for kids by default")
	_, b = c.do(http.MethodGet, "/api/widgets/info", "admin-session", nil)
	assert.Equal(t, false, decodeMap(t, b)["search"].(map[string]any)["kids"])
	code, _ = c.do(http.MethodPatch, "/api/admin/settings", "admin-session", map[string]any{"kidWebSearch": true})
	require.Equal(t, http.StatusOK, code)
	_, b = c.do(http.MethodGet, "/api/widgets/info", kidTok, nil)
	assert.Equal(t, true, decodeMap(t, b)["search"].(map[string]any)["enabled"])
	_, b = c.do(http.MethodGet, "/api/widgets/info", "admin-session", nil)
	assert.Equal(t, true, decodeMap(t, b)["search"].(map[string]any)["kids"])

	// A custom provider is named by its host.
	code, _ = c.do(http.MethodPatch, "/api/admin/settings", "admin-session", map[string]any{"webSearchUrl": "https://search.example.com/?q={query}"})
	require.Equal(t, http.StatusOK, code)
	_, b = c.do(http.MethodGet, "/api/widgets/info", "non-admin-session", nil)
	assert.Equal(t, "search.example.com", decodeMap(t, b)["search"].(map[string]any)["name"])
}

func TestWidgets_SettingsPatch(t *testing.T) {
	env := testsupport.NewEnv(t)
	c := &calls{t: t, base: env.URL}

	code, b := c.do(http.MethodPatch, "/api/admin/settings", "admin-session", map[string]any{"clockEnabled": true, "clockHour12": true})
	require.Equal(t, http.StatusOK, code)
	m := decodeMap(t, b)
	assert.Equal(t, true, m["showUptimeDisplay"], "the existing fields are still echoed")
	ws := m["widgets"].(map[string]any)
	assert.Equal(t, true, ws["clockEnabled"])
	assert.Equal(t, true, ws["clockHour12"])
	assert.Equal(t, false, ws["weatherEnabled"])
	assert.Equal(t, "", ws["weatherPlace"])
	assert.Equal(t, "c", ws["weatherUnits"])
	assert.Equal(t, false, ws["webSearchEnabled"])
	assert.Equal(t, "https://duckduckgo.com/?q={query}", ws["webSearchUrl"])
	assert.Equal(t, false, ws["kidWebSearch"])
	_, hasLat := ws["weatherLat"]
	assert.False(t, hasLat)

	code, b = c.do(http.MethodPatch, "/api/admin/settings", "admin-session", map[string]any{"clockHour12": nil})
	require.Equal(t, http.StatusOK, code)
	assert.Nil(t, decodeMap(t, b)["widgets"].(map[string]any)["clockHour12"], "null = the device's choice")
	code, b = c.do(http.MethodPatch, "/api/admin/settings", "admin-session", map[string]any{"showUptimeDisplay": false})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, decodeMap(t, b)["widgets"].(map[string]any)["clockEnabled"], "an omitted field keeps its value")

	for name, body := range map[string]map[string]any{
		"weather on without a place": {"weatherEnabled": true},
		"http search URL":            {"webSearchUrl": "http://duckduckgo.com/?q={query}"},
		"search URL without {query}": {"webSearchUrl": "https://duckduckgo.com/?q="},
		"units k":                    {"weatherUnits": "k"},
		"latitude out of range":      {"weatherPlace": "X", "weatherLat": 91, "weatherLon": 0},
		"longitude without latitude": {"weatherPlace": "X", "weatherLon": 4},
		"bad hour12":                 {"clockHour12": "yes"},
	} {
		code, _ := c.do(http.MethodPatch, "/api/admin/settings", "admin-session", body)
		assert.True(t, code == http.StatusUnprocessableEntity || code == http.StatusBadRequest, "%s: got %d", name, code)
	}

	// The public config doesn't grow the widget settings (coordinates!).
	code, b = c.do(http.MethodGet, "/api/system/config", "", nil)
	require.Equal(t, http.StatusOK, code)
	assert.NotContains(t, string(b), "weather")
	assert.NotContains(t, string(b), "widgets")

	code, _ = c.do(http.MethodPatch, "/api/admin/settings", "non-admin-session", map[string]any{"clockEnabled": true})
	assert.Equal(t, http.StatusForbidden, code)
}

func TestWidgets_Geocode(t *testing.T) {
	meteo := newFakeOpenMeteo(t)
	env := testsupport.NewEnvWidgets(t, nil, testsupport.WidgetOptions{GeocodeURL: meteo.URL + "/v1/search"})
	c := &calls{t: t, base: env.URL}

	code, b := c.do(http.MethodGet, "/api/admin/geocode?q=Rotterdam", "admin-session", nil)
	require.Equal(t, http.StatusOK, code)
	var places []map[string]any
	require.NoError(t, json.Unmarshal(b, &places))
	require.Len(t, places, 1)
	assert.Equal(t, map[string]any{"name": "Rotterdam", "region": "South Holland", "country": "Netherlands", "lat": 51.92, "lon": 4.48}, places[0])

	code, _ = c.do(http.MethodGet, "/api/admin/geocode?q=R", "admin-session", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, code)
	code, _ = c.do(http.MethodGet, "/api/admin/geocode?q=Rotterdam", "non-admin-session", nil)
	assert.Equal(t, http.StatusForbidden, code)

	down := testsupport.NewEnv(t) // weather points at a black hole
	code, b = (&calls{t: t, base: down.URL}).do(http.MethodGet, "/api/admin/geocode?q=Rotterdam", "admin-session", nil)
	assert.Equal(t, http.StatusBadGateway, code)
	assert.Equal(t, "unreachable", decodeMap(t, b)["error"])
}

func TestWidgets_ShowWidgetsPref(t *testing.T) {
	env := testsupport.NewEnv(t)
	code, me := getMeRaw(t, env.URL, "non-admin-session")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, me["showWidgets"], "on by default (AC-031)")

	code, me = patchMeJSON(t, env.URL, "non-admin-session", map[string]any{"showWidgets": false})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, false, me["showWidgets"])
	_, me = getMeRaw(t, env.URL, "non-admin-session")
	assert.Equal(t, false, me["showWidgets"])
	_, me = getMeRaw(t, env.URL, "admin-session")
	assert.Equal(t, true, me["showWidgets"], "another user is unchanged (TC-011)")

	_, kidTok := env.SeedKid(t, "Ava")
	code, _ = patchMeJSON(t, env.URL, kidTok, map[string]any{"showWidgets": false})
	assert.Equal(t, http.StatusForbidden, code, "a kid may change only themePref")
}

// fakeOpenMeteo is an httptest fake of Open-Meteo's forecast and geocoding.
type fakeOpenMeteoSrv struct {
	*httptest.Server
	q atomic.Value
}

func (f *fakeOpenMeteoSrv) lastQuery() url.Values {
	v, _ := f.q.Load().(url.Values)
	return v
}

func newFakeOpenMeteo(t *testing.T) *fakeOpenMeteoSrv {
	t.Helper()
	f := &fakeOpenMeteoSrv{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.q.Store(r.URL.Query())
		switch r.URL.Path {
		case "/v1/forecast":
			_, _ = w.Write([]byte(`{"current":{"temperature_2m":7.7,"weather_code":3,"is_day":0},"daily":{"temperature_2m_max":[9.0],"temperature_2m_min":[2.8]}}`))
		case "/v1/search":
			_, _ = w.Write([]byte(`{"results":[{"name":"Rotterdam","admin1":"South Holland","country":"Netherlands","latitude":51.9225,"longitude":4.47917}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}
