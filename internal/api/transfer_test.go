package api_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/gatus"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport/schemacheck"
)

// SPEC-backup-import — the five admin routes against a real Postgres.

type tPlan struct {
	PlanHash string `json:"planHash"`
	Mode     string `json:"mode"`
	Summary  struct {
		Create    map[string]int `json:"create"`
		Update    map[string]int `json:"update"`
		Delete    map[string]int `json:"delete"`
		Unchanged int            `json:"unchanged"`
		Skipped   int            `json:"skipped"`
	} `json:"summary"`
	Items []struct {
		Kind     string   `json:"kind"`
		Name     string   `json:"name"`
		Action   string   `json:"action"`
		Reason   string   `json:"reason"`
		Warnings []string `json:"warnings"`
	} `json:"items"`
	GatusSnippet  string `json:"gatusSnippet"`
	OtherAccounts int    `json:"otherAccounts"`
	Applied       bool   `json:"applied"`
}

type tError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Count   int    `json:"count"`
	Limit   int    `json:"limit"`
	Things  string `json:"things"`
	Plan    *tPlan `json:"plan"`
}

func (p tPlan) noChanges() bool {
	for _, m := range []map[string]int{p.Summary.Create, p.Summary.Update, p.Summary.Delete} {
		for _, n := range m {
			if n != 0 {
				return false
			}
		}
	}
	return true
}

func postRaw(t *testing.T, url, token string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "homepad_session", Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, b
}

func fixtureBytes(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "transfer", "testdata", rel))
	require.NoError(t, err)
	return b
}

func decodeAs[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(b, &v), "body: %s", b)
	return v
}

func previewRestore(t *testing.T, base, token, mode string, file []byte) tPlan {
	t.Helper()
	code, b := postRaw(t, base+"/api/admin/restore/preview?mode="+mode, token, file)
	require.Equal(t, http.StatusOK, code, "%s", b)
	return decodeAs[tPlan](t, b)
}

func applyRestore(t *testing.T, base, token, mode, hash string, file []byte) (int, []byte) {
	t.Helper()
	return postRaw(t, base+"/api/admin/restore?mode="+mode+"&plan="+hash, token, file)
}

func homepageBody(t *testing.T, services, bookmarks, settings string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]string{"services": services, "bookmarks": bookmarks, "settings": settings})
	require.NoError(t, err)
	return b
}

func previewImport(t *testing.T, base, token string, body []byte) tPlan {
	t.Helper()
	code, b := postRaw(t, base+"/api/admin/import/homepage/preview", token, body)
	require.Equal(t, http.StatusOK, code, "%s", b)
	return decodeAs[tPlan](t, b)
}

func applyImport(t *testing.T, base, token, hash string, body []byte) (int, []byte) {
	t.Helper()
	return postRaw(t, base+"/api/admin/import/homepage?plan="+hash, token, body)
}

func getExport(t *testing.T, base, token, query string) (*http.Response, []byte) {
	t.Helper()
	resp := doJSON(t, http.MethodGet, base+"/api/admin/export"+query, token, nil)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, b
}

// dumpDB renders every row of every table as JSON text, for greps.
func dumpDB(t *testing.T, env *testsupport.Env) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, env.DSN)
	require.NoError(t, err)
	defer conn.Close(ctx)
	var out strings.Builder
	for _, table := range []string{"users", "services", "categories", "library_apps", "service_icons", "system_settings",
		"favorites", "user_layout", "user_collapsed_categories", "kid_allowed_services", "sessions"} {
		var s *string
		require.NoError(t, conn.QueryRow(ctx, `SELECT string_agg(row_to_json(t)::text, E'\n') FROM `+table+` t`).Scan(&s), table)
		if s != nil {
			out.WriteString(*s + "\n")
		}
	}
	return out.String()
}

func countRows(t *testing.T, env *testsupport.Env, sql string, args ...any) int {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, env.DSN)
	require.NoError(t, err)
	defer conn.Close(ctx)
	var n int
	require.NoError(t, conn.QueryRow(ctx, sql, args...).Scan(&n))
	return n
}

// captureLog sends the standard logger to a buffer for the rest of the test.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func seedAdult(t *testing.T, env *testsupport.Env, email string) string {
	t.Helper()
	u, err := env.Store.CreateUser(context.Background(), email, "$2a$04$not-a-real-hash-but-looks-like-one", "user")
	require.NoError(t, err)
	return u.ID
}

// populate gives the fixture env a full catalog: sections, apps with icons,
// offers, settings, a kid, and per-person state, all through the API.
func populate(t *testing.T, env *testsupport.Env) {
	t.Helper()
	media := createCategory(t, env.URL, "admin-session", "Media")
	createCategory(t, env.URL, "admin-session", "Network")
	for _, id := range []string{firstServiceID(t, env.URL, "admin-session")} {
		r := putIcon(t, env.URL, "admin-session", id, "light", pngBytes(t, 32, 32))
		r.Body.Close()
		require.Equal(t, http.StatusNoContent, r.StatusCode)
		r = putIcon(t, env.URL, "admin-session", id, "dark", pngBytes(t, 48, 48))
		r.Body.Close()
		require.Equal(t, http.StatusNoContent, r.StatusCode)
		r = doJSON(t, http.MethodPatch, env.URL+"/api/services/"+id, "admin-session", map[string]any{"categoryId": media.ID})
		r.Body.Close()
		require.Equal(t, http.StatusOK, r.StatusCode)
	}
	createOffer(t, env.URL, "admin-session", "Immich", "https://immich.app")
	r := doJSON(t, http.MethodPatch, env.URL+"/api/admin/settings", "admin-session", map[string]any{"statusDegradedMs": 1500})
	r.Body.Close()
	require.Equal(t, http.StatusOK, r.StatusCode)
	env.SeedKid(t, "Maya", firstServiceID(t, env.URL, "admin-session"))
	r = doJSON(t, http.MethodPost, env.URL+"/api/favorites/"+firstServiceID(t, env.URL, "admin-session"), "non-admin-session", nil)
	r.Body.Close()
}

// AC-006 / TC-012 — all five routes are admin-only: 403 for a user and a kid,
// 401 signed out.
func TestTransfer_AdminOnly(t *testing.T) {
	env := testsupport.NewEnv(t)
	_, kidToken := env.SeedKid(t, "Maya")
	file := fixtureBytes(t, "backups/v1.json")
	imp := homepageBody(t, string(fixtureBytes(t, "homepage/tc008-services.yaml")), "", "")
	routes := []struct {
		method, path string
		body         []byte
	}{
		{http.MethodGet, "/api/admin/export", nil},
		{http.MethodPost, "/api/admin/restore/preview?mode=merge", file},
		{http.MethodPost, "/api/admin/restore?mode=merge&plan=x", file},
		{http.MethodPost, "/api/admin/import/homepage/preview", imp},
		{http.MethodPost, "/api/admin/import/homepage?plan=x", imp},
	}
	before := dumpDB(t, env)
	for _, rt := range routes {
		for who, want := range map[string]int{"non-admin-session": 403, kidToken: 403, "": 401} {
			req, _ := http.NewRequest(rt.method, env.URL+rt.path, bytes.NewReader(rt.body))
			if who != "" {
				req.AddCookie(&http.Cookie{Name: "homepad_session", Value: who})
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, want, resp.StatusCode, "%s %s as %q", rt.method, rt.path, who)
		}
	}
	assert.Equal(t, before, dumpDB(t, env), "refused requests change nothing")
}

// AC-001, AC-002, AC-003, AC-004 / TC-001, TC-002 — a backup of a fully
// populated DB: the headers, the schema, the whole catalog, and none of the
// secrets.
func TestTransfer_ExportFromAPopulatedDB(t *testing.T) {
	t.Setenv("OIDC_CLIENT_SECRET", "oidc-secret-marker-9f1e")
	env := testsupport.NewEnv(t)
	populate(t, env)

	resp, b := getExport(t, env.URL, "admin-session", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", b)
	assert.Equal(t, "application/json", strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Equal(t, fmt.Sprintf(`attachment; filename="homepad-backup-%s.json"`, time.Now().UTC().Format("2006-01-02")),
		resp.Header.Get("Content-Disposition"))

	schema, err := os.ReadFile(filepath.Join("..", "..", "docs", "schema", "homepad-backup-v1.json"))
	require.NoError(t, err)
	assert.Empty(t, schemacheck.Validate(schema, b))

	var f struct {
		IncludesPeople bool `json:"includesPeople"`
		Sections       []struct{ Name string }
		Apps           []struct {
			Slug          string
			Section       *string
			UploadedIcons map[string]map[string]string
		}
		Library  []struct{ Name string }
		Settings struct{ StatusDegradedMs *int }
		People   *struct {
			Adults []struct{ Email string }
			Kids   []struct {
				Name        string
				AllowedApps []string
			}
		}
	}
	require.NoError(t, json.Unmarshal(b, &f))
	assert.True(t, f.IncludesPeople)
	assert.Equal(t, []struct{ Name string }{{"Media"}, {"Network"}}, f.Sections)
	require.Len(t, f.Apps, 2, "the owner's catalog only; the user's legacy rows aren't the catalog")
	assert.Equal(t, "gitea", f.Apps[0].Slug)
	assert.Equal(t, "Media", *f.Apps[0].Section)
	assert.Len(t, f.Apps[0].UploadedIcons, 2, "both variants")
	assert.Equal(t, 1500, *f.Settings.StatusDegradedMs)
	require.NotNil(t, f.People)
	assert.Len(t, f.People.Adults, 3)
	require.Len(t, f.People.Kids, 1)
	assert.Equal(t, []string{"gitea"}, f.People.Kids[0].AllowedApps)
	var offerNames []string
	for _, o := range f.Library {
		offerNames = append(offerNames, o.Name)
	}
	assert.Contains(t, offerNames, "Immich")

	// AC-004 — none of these, anywhere in the file.
	body := string(b)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, env.DSN)
	require.NoError(t, err)
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `SELECT password_hash FROM users WHERE password_hash IS NOT NULL`)
	require.NoError(t, err)
	var hashes []string
	for rows.Next() {
		var h string
		require.NoError(t, rows.Scan(&h))
		hashes = append(hashes, h)
	}
	require.NotEmpty(t, hashes)
	for _, h := range hashes {
		assert.NotContains(t, body, h, "a password hash")
	}
	assert.NotContains(t, body, "$2a$")
	rows, err = conn.Query(ctx, `SELECT token_hash FROM sessions`)
	require.NoError(t, err)
	n := 0
	for rows.Next() {
		var th []byte
		require.NoError(t, rows.Scan(&th))
		assert.NotContains(t, body, hex.EncodeToString(th), "a session hash")
		n++
	}
	require.Greater(t, n, 0)
	for _, tok := range []string{"admin-session", "non-admin-session", "kid-session-"} {
		assert.NotContains(t, body, tok, "a session token")
	}
	for _, s := range []string{"oidc-secret-marker-9f1e", "postgres://", "127.0.0.1:1", "password", "token"} {
		assert.NotContains(t, body, s)
	}
	uuid := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	assert.Empty(t, uuid.FindAllString(body, -1), "no database id")

	// TC-002 — without people: no people key and no "@" anywhere.
	resp, b = getExport(t, env.URL, "admin-session", "?people=false")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotContains(t, string(b), `"people"`)
	assert.NotContains(t, string(b), "@")
	assert.Empty(t, schemacheck.Validate(schema, b))

	resp, _ = getExport(t, env.URL, "admin-session", "?people=maybe")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// AC-005 — two backups of an unchanged dashboard differ only in exportedAt.
func TestTransfer_ExportIsDeterministic(t *testing.T) {
	env := testsupport.NewEnv(t)
	populate(t, env)
	_, a := getExport(t, env.URL, "admin-session", "")
	time.Sleep(1100 * time.Millisecond)
	_, b := getExport(t, env.URL, "admin-session", "")
	re := regexp.MustCompile(`"exportedAt": "[^"]+"`)
	assert.Equal(t, string(re.ReplaceAll(a, nil)), string(re.ReplaceAll(b, nil)))
}

// AC-014, AC-012, AC-013, AC-015 / TC-003, TC-004 — the fixture restored onto
// a fresh install, as a second admin (AC-025 / TC-011). The kid is created
// with no password and no devices; the second preview has nothing to change.
func TestTransfer_RestoreOntoAFreshInstall(t *testing.T) {
	env := testsupport.NewEnv(t)
	emptyCatalog(t, env)
	second := seedSecondAdmin(t, env)
	alexID := seedAdult(t, env, "alex@example.com")
	file := fixtureBytes(t, "backups/v1.json")
	logs := captureLog(t)

	before := dumpDB(t, env)
	p := previewRestore(t, env.URL, second, "merge", file)
	assert.Equal(t, before, dumpDB(t, env), "AC-007: a preview changes nothing")
	assert.Equal(t, 3, p.Summary.Create["sections"])
	assert.Equal(t, 5, p.Summary.Create["apps"])
	assert.Equal(t, 3, p.Summary.Create["icons"])
	assert.Equal(t, 2, p.Summary.Create["offers"])
	assert.Equal(t, 1, p.Summary.Create["kids"])
	assert.Equal(t, 1, p.Summary.Skipped, "sam has no account")

	code, b := applyRestore(t, env.URL, second, "merge", p.PlanHash, file)
	require.Equal(t, http.StatusOK, code, "%s", b)
	res := decodeAs[tPlan](t, b)
	assert.True(t, res.Applied)
	assert.Empty(t, res.PlanHash)

	// Every write went to the shared catalog owner (AC-025).
	assert.Equal(t, 5, countRows(t, env, `SELECT count(*) FROM services WHERE user_id = $1 AND slug = ANY($2)`,
		env.AdminID, []string{"grafana", "jellyfin", "router", "sonarr", "unifi"}))
	assert.Equal(t, 3, countRows(t, env, `SELECT count(*) FROM categories WHERE user_id = $1 AND name = ANY($2)`,
		env.AdminID, []string{"Media", "Network", "Tools"}))
	assert.Equal(t, 0, countRows(t, env, `SELECT count(*) FROM services s JOIN users u ON u.id = s.user_id WHERE u.email = 'admin2@homepad.test'`))
	// A plain user sees the restored apps.
	var slugs []string
	for _, sv := range listServicesFull(t, env.URL, "non-admin-session") {
		slugs = append(slugs, sv.Slug)
	}
	assert.Subset(t, slugs, []string{"grafana", "jellyfin", "router", "sonarr", "unifi"})
	// Icons are served, with the file's hash as the ETag (AC-015).
	var jid string
	for _, sv := range listServicesFull(t, env.URL, "admin-session") {
		if sv.Slug == "jellyfin" {
			jid = sv.ID
		}
	}
	ic := doJSON(t, http.MethodGet, env.URL+"/api/services/"+jid+"/icon/light", "non-admin-session", nil)
	ic.Body.Close()
	assert.Equal(t, http.StatusOK, ic.StatusCode)
	assert.Equal(t, `"55a202d33848083e4cfafa505607b003fd5a8df6055e0288a055dc639bd423f1"`, ic.Header.Get("ETag"))
	// The kid: no email, no password, no devices (AC-012).
	assert.Equal(t, 1, countRows(t, env, `SELECT count(*) FROM users WHERE role = 'kid' AND display_name = 'Maya'
		AND email IS NULL AND password_hash IS NULL AND avatar_color = 'teal'`))
	assert.Equal(t, 0, countRows(t, env, `SELECT count(*) FROM sessions s JOIN users u ON u.id = s.user_id WHERE u.display_name = 'Maya'`))
	assert.Equal(t, 2, countRows(t, env, `SELECT count(*) FROM kid_allowed_services k JOIN users u ON u.id = k.user_id WHERE u.display_name = 'Maya'`))
	// No adult was created (sam), and Alex's dashboard came back.
	assert.Equal(t, 0, countRows(t, env, `SELECT count(*) FROM users WHERE email = 'sam@example.com'`))
	assert.Equal(t, 1, countRows(t, env, `SELECT count(*) FROM favorites f JOIN services s ON s.id = f.service_id WHERE f.user_id = $1 AND s.slug = 'jellyfin'`, alexID))
	assert.Equal(t, 3, countRows(t, env, `SELECT count(*) FROM user_layout WHERE user_id = $1`, alexID))
	assert.Equal(t, 1, countRows(t, env, `SELECT count(*) FROM user_collapsed_categories WHERE user_id = $1`, alexID))
	assert.Equal(t, 1, countRows(t, env, `SELECT count(*) FROM users WHERE id = $1 AND theme_pref = 'dark' AND display_name = 'Alex'`, alexID))
	assert.Equal(t, 1, countRows(t, env, `SELECT count(*) FROM system_settings WHERE status_degraded_ms = 1500`))

	// AC-030 — one log line per apply: who, mode, counts. No file contents.
	var lines []string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "transfer:") {
			lines = append(lines, l)
		}
	}
	require.Len(t, lines, 1, "%s", logs.String())
	assert.Contains(t, lines[0], "restore")
	assert.Contains(t, lines[0], "mode=merge")
	assert.Contains(t, lines[0], "admin2@homepad.test")
	assert.Contains(t, lines[0], "apps=5")
	for _, s := range []string{"Jellyfin", "jellyfin.example.com", "alex@example.com", "iVBOR"} {
		assert.NotContains(t, logs.String(), s)
	}

	// TC-004 — the second preview: nothing to change.
	again := previewRestore(t, env.URL, second, "merge", file)
	assert.True(t, again.noChanges(), "%+v", again.Summary)
}

// AC-011 / TC-005 — Replace deletes what isn't in the file, and the preview
// lists it.
func TestTransfer_RestoreReplace(t *testing.T) {
	env := testsupport.NewEnv(t)
	old := env.SeedService(t, "old-thing", "Old thing", "")
	file := fixtureBytes(t, "backups/v1.json")
	p := previewRestore(t, env.URL, "admin-session", "replace", file)
	var deleted []string
	for _, it := range p.Items {
		if it.Action == "delete" {
			deleted = append(deleted, it.Name)
		}
	}
	assert.Contains(t, deleted, "Old thing")
	assert.Equal(t, 3, p.Summary.Delete["apps"], "gitea, grafana and Old thing")

	code, b := applyRestore(t, env.URL, "admin-session", "replace", p.PlanHash, file)
	require.Equal(t, http.StatusOK, code, "%s", b)
	assert.Equal(t, 0, countRows(t, env, `SELECT count(*) FROM services WHERE id = $1`, old))
	assert.Equal(t, 5, countRows(t, env, `SELECT count(*) FROM services WHERE user_id = $1`, env.AdminID))
	assert.Equal(t, 2, countRows(t, env, `SELECT count(*) FROM services WHERE user_id = $1`, env.UserID), "rows outside the catalog are not the catalog")
	assert.Equal(t, 2, countRows(t, env, `SELECT count(*) FROM library_apps`))

	code, b = postRaw(t, env.URL+"/api/admin/restore/preview?mode=wipe", "admin-session", file)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "bad_mode", decodeAs[tError](t, b).Code)
	code, b = postRaw(t, env.URL+"/api/admin/restore?mode=merge", "admin-session", file)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "plan_missing", decodeAs[tError](t, b).Code)
}

// AC-008 / TC-006 — a stale preview gets 409 with the new plan, and nothing
// changes.
func TestTransfer_StalePreviewIs409(t *testing.T) {
	env := testsupport.NewEnv(t)
	file := fixtureBytes(t, "backups/v1.json")
	p := previewRestore(t, env.URL, "admin-session", "merge", file)
	env.SeedService(t, "sonarr", "Sonarr", "") // "another tab"
	before := dumpDB(t, env)

	code, b := applyRestore(t, env.URL, "admin-session", "merge", p.PlanHash, file)
	require.Equal(t, http.StatusConflict, code, "%s", b)
	e := decodeAs[tError](t, b)
	assert.Equal(t, "plan_changed", e.Code)
	require.NotNil(t, e.Plan, "the new plan comes back, ready to apply")
	assert.NotEqual(t, p.PlanHash, e.Plan.PlanHash)
	assert.Equal(t, before, dumpDB(t, env), "nothing changed")

	code, b = applyRestore(t, env.URL, "admin-session", "merge", e.Plan.PlanHash, file)
	assert.Equal(t, http.StatusOK, code, "%s", b)

	imp := homepageBody(t, string(fixtureBytes(t, "homepage/tc008-services.yaml")), "", "")
	ip := previewImport(t, env.URL, "admin-session", imp)
	env.SeedService(t, "plex-elsewhere", "Plex", "")
	r := doJSON(t, http.MethodPatch, env.URL+"/api/services/"+firstSlugID(t, env, "plex-elsewhere"), "admin-session", map[string]any{"url": "http://plex.home.arpa/"})
	r.Body.Close()
	code, b = applyImport(t, env.URL, "admin-session", ip.PlanHash, imp)
	assert.Equal(t, http.StatusConflict, code, "%s", b)
}

// emptyCatalog deletes the fixture's two catalog apps, leaving a fresh
// install's empty catalog.
func emptyCatalog(t *testing.T, env *testsupport.Env) {
	t.Helper()
	for _, sv := range listServicesFull(t, env.URL, "admin-session") {
		r := doJSON(t, http.MethodDelete, env.URL+"/api/services/"+sv.ID, "admin-session", nil)
		r.Body.Close()
		require.Equal(t, http.StatusNoContent, r.StatusCode)
	}
	require.Equal(t, 0, countRows(t, env, `SELECT count(*) FROM services WHERE user_id = $1`, env.AdminID))
}

func firstSlugID(t *testing.T, env *testsupport.Env, slug string) string {
	t.Helper()
	for _, sv := range listServicesFull(t, env.URL, "admin-session") {
		if sv.Slug == slug {
			return sv.ID
		}
	}
	t.Fatalf("no service %s", slug)
	return ""
}

// AC-009 — an error part-way through an apply leaves the database as it was.
func TestTransfer_ApplyIsAllOrNothing(t *testing.T) {
	env := testsupport.NewEnv(t)
	seedAdult(t, env, "alex@example.com")
	h := api.NewWithTransferFault(api.Deps{
		Store:    env.Store,
		Poller:   gatus.NewPoller(gatus.NewClient("http://127.0.0.1:1"), time.Hour),
		Sessions: env.Sessions,
	}, 6)
	faulty := httptest.NewServer(h)
	t.Cleanup(faulty.Close)

	file := fixtureBytes(t, "backups/v1.json")
	p := previewRestore(t, faulty.URL, "admin-session", "replace", file)
	before := dumpDB(t, env)
	code, b := applyRestore(t, faulty.URL, "admin-session", "replace", p.PlanHash, file)
	require.Equal(t, http.StatusInternalServerError, code, "%s", b)
	assert.Equal(t, before, dumpDB(t, env), "the delete and the first writes rolled back")

	imp := homepageBody(t, string(fixtureBytes(t, "homepage/tc008-services.yaml")), "", "")
	ip := previewImport(t, faulty.URL, "admin-session", imp)
	code, _ = applyImport(t, faulty.URL, "admin-session", ip.PlanHash, imp)
	require.Equal(t, http.StatusInternalServerError, code)
	assert.Equal(t, before, dumpDB(t, env))
}

// AC-009 — two applies at once are serialized: one lands, the other sees the
// changed catalog and gets 409. Nothing is imported twice.
func TestTransfer_ConcurrentAppliesAreSerialized(t *testing.T) {
	env := testsupport.NewEnv(t)
	imp := homepageBody(t, string(fixtureBytes(t, "homepage/tc008-services.yaml")), "", "")
	p := previewImport(t, env.URL, "admin-session", imp)
	codes := make([]int, 4)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = applyImport(t, env.URL, "admin-session", p.PlanHash, imp)
		}(i)
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		}
	}
	assert.Equal(t, 1, ok, "%v", codes)
	assert.Equal(t, 3, conflict, "%v", codes)
	assert.Equal(t, 1, countRows(t, env, `SELECT count(*) FROM services WHERE user_id = $1 AND name = 'Sonarr'`, env.AdminID))
}

// AC-016, AC-026, AC-028 — refusals and their status codes.
func TestTransfer_Refusals(t *testing.T) {
	env := testsupport.NewEnv(t)
	cases := []struct {
		name   string
		body   []byte
		status int
		code   string
	}{
		{"not a backup", []byte(`{"format":"homarr","schemaVersion":1}`), 422, "not_a_backup"},
		{"newer", []byte(`{"format":"homepad-backup","schemaVersion":99}`), 422, "schema_newer"},
		{"malformed", []byte("{\n\"format\": \"homepad-backup\",\n\"apps\": [,]\n}"), 400, "invalid_json"},
	}
	for _, c := range cases {
		code, b := postRaw(t, env.URL+"/api/admin/restore/preview?mode=merge", "admin-session", c.body)
		assert.Equal(t, c.status, code, c.name)
		e := decodeAs[tError](t, b)
		assert.Equal(t, c.code, e.Code, c.name)
		if c.code == "invalid_json" {
			assert.Equal(t, 3, e.Line)
		}
		if c.code == "schema_newer" {
			assert.Equal(t, "This backup is from a newer Homepad. Update Homepad, then try again.", e.Message)
		}
	}

	// A body over 16 MB is a 413.
	big := append([]byte(`{"format":"homepad-backup","schemaVersion":1,"pad":"`), bytes.Repeat([]byte("x"), 16<<20)...)
	big = append(big, []byte(`"}`)...)
	// Expect: 100-continue lets the server refuse before the client sends
	// 16 MB, so the response can't be lost to a reset connection.
	req, err := http.NewRequest(http.MethodPost, env.URL+"/api/admin/restore/preview?mode=merge", bytes.NewReader(big))
	require.NoError(t, err)
	req.Header.Set("Expect", "100-continue")
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	assert.Equal(t, "too_large", decodeAs[tError](t, b).Code)
	var code int

	// A Homepage file over 1 MB is a 413; a bad one is a 400/422 with a line.
	code, b = postRaw(t, env.URL+"/api/admin/import/homepage/preview", "admin-session",
		homepageBody(t, "- A:\n    - B:\n        href: https://b.example.com\n        description: "+strings.Repeat("x", 1<<20)+"\n", "", ""))
	assert.Equal(t, http.StatusRequestEntityTooLarge, code)
	e := decodeAs[tError](t, b)
	assert.Equal(t, "too_large", e.Code)
	assert.Equal(t, "services", e.File)

	code, b = postRaw(t, env.URL+"/api/admin/import/homepage/preview", "admin-session",
		homepageBody(t, string(fixtureBytes(t, "homepage/docker-compose.yaml")), "", ""))
	assert.Equal(t, http.StatusUnprocessableEntity, code)
	e = decodeAs[tError](t, b)
	assert.Equal(t, "not_homepage_services", e.Code)
	assert.Equal(t, 1, e.Line)

	code, b = postRaw(t, env.URL+"/api/admin/import/homepage/preview", "admin-session",
		homepageBody(t, "- Media:\n    - A:\n        href: [oops\n", "", ""))
	assert.Equal(t, http.StatusBadRequest, code)
	e = decodeAs[tError](t, b)
	assert.Equal(t, "yaml_syntax", e.Code)
	assert.Greater(t, e.Line, 0)

	code, _ = postRaw(t, env.URL+"/api/admin/import/homepage/preview", "admin-session", []byte(`{"services": 3}`))
	assert.Equal(t, http.StatusBadRequest, code)

	var apps strings.Builder
	apps.WriteString("- Media:\n")
	for i := 0; i <= 1000; i++ {
		fmt.Fprintf(&apps, "    - App %d:\n        href: https://a%d.example.com\n", i, i)
	}
	code, b = postRaw(t, env.URL+"/api/admin/import/homepage/preview", "admin-session", homepageBody(t, apps.String(), "", ""))
	assert.Equal(t, http.StatusUnprocessableEntity, code)
	e = decodeAs[tError](t, b)
	assert.Equal(t, "too_many", e.Code)
	assert.Equal(t, 1001, e.Count)
	assert.Equal(t, 1000, e.Limit)
}

// TC-008, AC-022, AC-023, AC-029 — the Homepage happy path as a second admin
// (AC-025), then TC-009: Gatus learns the keys, the same import turns their
// status on. The widget key is nowhere: not in the DB, the logs, or any
// response.
func TestTransfer_HomepageImport(t *testing.T) {
	env := testsupport.NewEnvWithGatus(t, map[string][]testsupport.GatusResult{
		"media_sonarr": {{Success: true, DurationMs: 12}},
		"infra_router": {{Success: true, DurationMs: 3}},
	})
	second := seedSecondAdmin(t, env)
	logs := captureLog(t)
	var responses []string

	imp := homepageBody(t, string(fixtureBytes(t, "homepage/tc008-services.yaml")),
		string(fixtureBytes(t, "homepage/bookmarks.yaml")), string(fixtureBytes(t, "homepage/settings.yaml")))
	code, b := postRaw(t, env.URL+"/api/admin/import/homepage/preview", second, imp)
	require.Equal(t, http.StatusOK, code, "%s", b)
	responses = append(responses, string(b))
	p := decodeAs[tPlan](t, b)
	assert.Equal(t, 7, p.Summary.Create["apps"], "4 services and 3 bookmarks")
	assert.Contains(t, p.GatusSnippet, "Sonarr", "the poller hasn't run, so Gatus knows no keys yet")
	assert.Equal(t, 3, p.OtherAccounts, "admin, user and alice")

	code, b = applyImport(t, env.URL, second, p.PlanHash, imp)
	require.Equal(t, http.StatusOK, code, "%s", b)
	responses = append(responses, string(b))

	svcs := listServicesFull(t, env.URL, "non-admin-session")
	bySlug := map[string]bool{}
	for _, sv := range svcs {
		bySlug[sv.Slug] = true
	}
	for _, s := range []string{"sonarr", "plex", "router", "proxmox", "github", "docs", "reddit"} {
		assert.True(t, bySlug[s], "%s imported and visible to a user", s)
	}
	assert.Equal(t, 0, countRows(t, env, `SELECT count(*) FROM services WHERE gatus_key IS NOT NULL AND user_id = $1 AND slug IN ('sonarr','router')`, env.AdminID))
	// AC-029 — the second admin's order holds the imports in Homepage order.
	secondID := ""
	for _, a := range listAccountsRaw(t, env) {
		if a.Email == "admin2@homepad.test" {
			secondID = a.ID
		}
	}
	require.NotEmpty(t, secondID)
	assert.Equal(t, 9, countRows(t, env, `SELECT count(*) FROM user_layout WHERE user_id = $1`, secondID), "2 existing + 7 imported")
	assert.Equal(t, 0, countRows(t, env, `SELECT count(*) FROM user_layout WHERE user_id = $1`, env.AdminID), "the owner's order is untouched")

	// TC-009 — Gatus now knows the keys.
	r := doJSON(t, http.MethodPost, env.URL+"/api/status/refresh", second, nil)
	r.Body.Close()
	require.Equal(t, http.StatusOK, r.StatusCode)
	p2 := previewImport(t, env.URL, second, imp)
	assert.Equal(t, 0, p2.Summary.Create["apps"])
	assert.Equal(t, 2, p2.Summary.Update["apps"])
	code, b = applyImport(t, env.URL, second, p2.PlanHash, imp)
	require.Equal(t, http.StatusOK, code, "%s", b)
	responses = append(responses, string(b))
	assert.Equal(t, 2, countRows(t, env, `SELECT count(*) FROM services WHERE user_id = $1 AND gatus_key IN ('media_sonarr','infra_router')`, env.AdminID))
	_, sb := getBody(t, env.URL+"/api/services", "non-admin-session")
	responses = append(responses, string(sb))
	assert.Contains(t, string(sb), `"status":"UP"`)

	// AC-022 — the widget key and the provider key, nowhere.
	db := dumpDB(t, env)
	for _, secret := range []string{"abc123", "fixture-provider-key-55ab"} {
		assert.NotContains(t, db, secret)
		assert.NotContains(t, logs.String(), secret)
		for _, resp := range responses {
			assert.NotContains(t, resp, secret)
		}
	}
	assert.Equal(t, 2, strings.Count(logs.String(), "transfer: import"), "one line per apply")
}

type rawAccount struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

func listAccountsRaw(t *testing.T, env *testsupport.Env) []rawAccount {
	t.Helper()
	code, b := getBody(t, env.URL+"/api/admin/accounts", "admin-session")
	require.Equal(t, http.StatusOK, code)
	var out struct {
		Accounts []rawAccount `json:"accounts"`
	}
	if err := json.Unmarshal(b, &out); err == nil && out.Accounts != nil {
		return out.Accounts
	}
	var list []rawAccount
	require.NoError(t, json.Unmarshal(b, &list))
	return list
}
