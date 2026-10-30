package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-kid-accounts AC-003…AC-006 — server-side filtering for kids. Every read
// that returns catalog data goes through storage.VisibleServiceIDs; a service
// off the kid's allow-list is never in a response body.

// kidWorld is a shared catalog with one app the kid may see and several it may
// not, plus a live Gatus snapshot covering all of them, so a leak through
// /api/status would actually have something to leak.
type kidWorld struct {
	env      *testsupport.Env
	kidID    string
	kidToken string

	allowedID string // "Kid Allowed" in category "Kid Media"
	hiddenID  string // "Secret App" in category "Secret Cat"
	grafanaID string // fixture service, moved into "Kid Media" (mixed category)
	giteaID   string // fixture service, uncategorized

	kidMediaCat string // contains allowed + hidden → visible
	secretCat   string // contains only hidden → hidden
	emptyCat    string // contains nothing → hidden
}

// hiddenMarkers are strings that exist only on services/categories the kid
// must not see. None may ever appear in a kid's response body.
func (w kidWorld) hiddenMarkers() []string {
	return []string{
		w.hiddenID, w.grafanaID, w.giteaID,
		"Secret App", "secret-app", "secret.example.com", "secret_key",
		"core_grafana", "core_gitea", "Grafana", "Gitea",
		"Secret Cat", "Empty Cat", w.secretCat, w.emptyCat,
	}
}

func assertNoHiddenMarkers(t *testing.T, w kidWorld, what string, body []byte) {
	t.Helper()
	var leaked []string
	for _, m := range w.hiddenMarkers() {
		if strings.Contains(string(body), m) {
			leaked = append(leaked, m)
		}
	}
	assert.Empty(t, leaked, "%s leaked hidden data to the kid", what)
}

func newKidWorld(t *testing.T) kidWorld {
	t.Helper()
	env := testsupport.NewEnvWithGatus(t, map[string][]testsupport.GatusResult{
		"core_gitea":   {{Success: true, DurationMs: 10}},
		"core_grafana": {{Success: true, DurationMs: 10}},
		"kid_ok_key":   {{Success: true, DurationMs: 10}},
		"secret_key":   {{Success: false, DurationMs: 10}},
	})
	w := kidWorld{env: env}

	admin := listServices(t, env.URL, "admin-session")
	for _, sv := range listServicesFull(t, env.URL, "admin-session") {
		switch sv.Slug {
		case "gitea":
			w.giteaID = sv.ID
		case "grafana":
			w.grafanaID = sv.ID
		}
	}
	require.Len(t, admin, 2)
	require.NotEmpty(t, w.giteaID)
	require.NotEmpty(t, w.grafanaID)

	w.allowedID = env.SeedService(t, "kid-allowed", "Kid Allowed", "kid_ok_key")
	w.hiddenID = env.SeedService(t, "secret-app", "Secret App", "secret_key")
	// SeedService uses https://<slug>.example.com — give the hidden one a
	// distinctive URL too.
	patch := doJSON(t, http.MethodPatch, env.URL+"/api/services/"+w.hiddenID, "admin-session",
		map[string]any{"url": "https://secret.example.com"})
	patch.Body.Close()
	require.Equal(t, http.StatusOK, patch.StatusCode)

	w.kidMediaCat = createCategory(t, env.URL, "admin-session", "Kid Media").ID
	w.secretCat = createCategory(t, env.URL, "admin-session", "Secret Cat").ID
	w.emptyCat = createCategory(t, env.URL, "admin-session", "Empty Cat").ID
	for id, cat := range map[string]string{w.allowedID: w.kidMediaCat, w.grafanaID: w.kidMediaCat, w.hiddenID: w.secretCat} {
		r := doJSON(t, http.MethodPatch, env.URL+"/api/services/"+id, "admin-session", map[string]any{"categoryId": cat})
		r.Body.Close()
		require.Equal(t, http.StatusOK, r.StatusCode)
	}

	// Populate the Gatus snapshot (an adult's refresh).
	r := doJSON(t, http.MethodPost, env.URL+"/api/status/refresh", "admin-session", nil)
	r.Body.Close()
	require.Equal(t, http.StatusOK, r.StatusCode)

	w.kidID, w.kidToken = env.SeedKid(t, "Sam", w.allowedID)
	return w
}

type fullService struct {
	ID           string  `json:"id"`
	Slug         string  `json:"slug"`
	Name         string  `json:"name"`
	Favorite     bool    `json:"favorite"`
	CategoryID   *string `json:"categoryId"`
	CategoryName *string `json:"categoryName"`
	GatusKey     string  `json:"gatus_key"`
}

func listServicesFull(t *testing.T, baseURL, token string) []fullService {
	t.Helper()
	code, body := getBody(t, baseURL+"/api/services", token)
	require.Equal(t, http.StatusOK, code)
	var payload struct {
		Services []fullService `json:"services"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	return payload.Services
}

func getBody(t *testing.T, url, token string) (int, []byte) {
	t.Helper()
	resp := doJSON(t, http.MethodGet, url, token, nil)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, b
}

// AC-003 — GET /api/services returns only allowed services.
func TestKid_ServicesFiltered(t *testing.T) {
	w := newKidWorld(t)

	code, body := getBody(t, w.env.URL+"/api/services", w.kidToken)
	require.Equal(t, http.StatusOK, code)
	assertNoHiddenMarkers(t, w, "GET /api/services", body)

	svcs := listServicesFull(t, w.env.URL, w.kidToken)
	require.Len(t, svcs, 1)
	assert.Equal(t, w.allowedID, svcs[0].ID)
	assert.Equal(t, "Kid Allowed", svcs[0].Name)
	require.NotNil(t, svcs[0].CategoryName)
	assert.Equal(t, "Kid Media", *svcs[0].CategoryName)

	// AC-018 — adults are unaffected: they still see the whole catalog.
	for _, tok := range []string{"admin-session", "non-admin-session"} {
		assert.Len(t, listServicesFull(t, w.env.URL, tok), 4, "%s sees every service", tok)
	}
}

// AC-004 — GET /api/status carries only the Gatus keys of allowed services.
func TestKid_StatusFiltered(t *testing.T) {
	w := newKidWorld(t)

	code, body := getBody(t, w.env.URL+"/api/status", w.kidToken)
	require.Equal(t, http.StatusOK, code)
	assertNoHiddenMarkers(t, w, "GET /api/status", body)
	var st struct {
		Statuses map[string]string `json:"statuses"`
	}
	require.NoError(t, json.Unmarshal(body, &st))
	assert.Equal(t, []string{"kid_ok_key"}, keysOf(st.Statuses))

	// Adults still get every key in the snapshot.
	_, body = getBody(t, w.env.URL+"/api/status", "non-admin-session")
	require.NoError(t, json.Unmarshal(body, &st))
	assert.Equal(t, []string{"core_gitea", "core_grafana", "kid_ok_key", "secret_key"}, keysOf(st.Statuses))
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AC-005 / OQ-2 — GET /api/categories returns only categories holding at least
// one allowed service.
func TestKid_CategoriesFiltered(t *testing.T) {
	w := newKidWorld(t)

	code, body := getBody(t, w.env.URL+"/api/categories", w.kidToken)
	require.Equal(t, http.StatusOK, code)
	assertNoHiddenMarkers(t, w, "GET /api/categories", body)
	cats := getCategories(t, w.env.URL, w.kidToken)
	require.Len(t, cats, 1)
	assert.Equal(t, w.kidMediaCat, cats[0].ID)
	assert.Equal(t, "Kid Media", cats[0].Name)

	assert.Len(t, getCategories(t, w.env.URL, "non-admin-session"), 3, "adults see every category, empty ones included")
}

// AC-006 — icon reads: an allowed service's icon is served; a hidden one is
// 404, indistinguishable from a service that does not exist.
func TestKid_IconReads(t *testing.T) {
	w := newKidWorld(t)
	png := pngBytes(t, 32, 32)
	for _, id := range []string{w.allowedID, w.hiddenID} {
		r := putIcon(t, w.env.URL, "admin-session", id, "light", png)
		r.Body.Close()
		require.Equal(t, http.StatusNoContent, r.StatusCode)
	}

	code, body := getBody(t, w.env.URL+"/api/services/"+w.allowedID+"/icon/light", w.kidToken)
	assert.Equal(t, http.StatusOK, code, "the kid can read an allowed service's icon")
	assert.True(t, bytes.Equal(png, body), "the icon bytes are served")

	hiddenCode, hiddenBody := getBody(t, w.env.URL+"/api/services/"+w.hiddenID+"/icon/light", w.kidToken)
	assert.Equal(t, http.StatusNotFound, hiddenCode, "AC-006: off-list icon read is 404")
	missingCode, missingBody := getBody(t, w.env.URL+"/api/services/00000000-0000-0000-0000-000000000000/icon/light", w.kidToken)
	assert.Equal(t, http.StatusNotFound, missingCode)
	assert.Equal(t, string(missingBody), string(hiddenBody), "a hidden service reads exactly like a missing one")
	badCode, _ := getBody(t, w.env.URL+"/api/services/not-a-uuid/icon/light", w.kidToken)
	assert.Equal(t, http.StatusNotFound, badCode)
	// An allowed service's missing variant is a plain 404 too.
	darkCode, _ := getBody(t, w.env.URL+"/api/services/"+w.allowedID+"/icon/dark", w.kidToken)
	assert.Equal(t, http.StatusNotFound, darkCode)
}

// AC-006 — favorites writes on an off-list service are 404 (both verbs); on an
// allowed service they work and persist (TC-006).
func TestKid_Favorites(t *testing.T) {
	w := newKidWorld(t)
	url := w.env.URL + "/api/favorites/"

	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		hidden := doJSON(t, method, url+w.hiddenID, w.kidToken, nil)
		hb, _ := io.ReadAll(hidden.Body)
		hidden.Body.Close()
		missing := doJSON(t, method, url+"00000000-0000-0000-0000-000000000000", w.kidToken, nil)
		mb, _ := io.ReadAll(missing.Body)
		missing.Body.Close()
		assert.Equal(t, http.StatusNotFound, hidden.StatusCode, "%s off-list favorite → 404", method)
		assert.Equal(t, http.StatusNotFound, missing.StatusCode, "%s unknown favorite → 404 for a kid", method)
		assert.Equal(t, string(mb), string(hb), "%s: hidden reads exactly like missing", method)
	}
	var n int
	require.NoError(t, w.env.Store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM favorites WHERE user_id = $1`, w.kidID).Scan(&n))
	assert.Zero(t, n, "no favorite row was written for a hidden service")

	add := doJSON(t, http.MethodPost, url+w.allowedID, w.kidToken, nil)
	add.Body.Close()
	assert.Equal(t, http.StatusNoContent, add.StatusCode)
	svcs := listServicesFull(t, w.env.URL, w.kidToken)
	require.Len(t, svcs, 1)
	assert.True(t, svcs[0].Favorite, "TC-006: the kid's favorite persists")

	del := doJSON(t, http.MethodDelete, url+w.allowedID, w.kidToken, nil)
	del.Body.Close()
	assert.Equal(t, http.StatusNoContent, del.StatusCode)
	assert.False(t, listServicesFull(t, w.env.URL, w.kidToken)[0].Favorite)
}

// The kid's collapsed-sections read is filtered to visible categories too.
func TestKid_CollapsedCategoriesFiltered(t *testing.T) {
	w := newKidWorld(t)
	require.NoError(t, w.env.Store.SetCollapsedCategories(context.Background(), w.kidID,
		[]string{w.kidMediaCat, w.secretCat, w.emptyCat}))

	code, body := getBody(t, w.env.URL+"/api/me/collapsed-categories", w.kidToken)
	require.Equal(t, http.StatusOK, code)
	assertNoHiddenMarkers(t, w, "GET /api/me/collapsed-categories", body)
	_, ids := getCollapsed(t, w.env.URL, w.kidToken)
	assert.Equal(t, []string{w.kidMediaCat}, ids)
}

// UX §5 — a kid with zero apps gets empty (not null, not everything) lists.
func TestKid_ZeroAppsSeesNothing(t *testing.T) {
	w := newKidWorld(t)
	_, empty := w.env.SeedKid(t, "Leo")

	for _, ep := range []string{"/api/services", "/api/categories", "/api/status", "/api/me/collapsed-categories"} {
		code, body := getBody(t, w.env.URL+ep, empty)
		require.Equal(t, http.StatusOK, code, ep)
		assertNoHiddenMarkers(t, w, ep, body)
		assert.NotContains(t, string(body), w.allowedID, ep)
		assert.NotContains(t, string(body), "null", "%s: empty lists are [] / {}, never null", ep)
	}
	assert.Empty(t, listServicesFull(t, w.env.URL, empty))
	assert.Empty(t, getCategories(t, w.env.URL, empty))
}

// AC-002 — an allow-list change applies on the kid's next read, with no
// sign-out.
func TestKid_AllowListChangeAppliesOnNextRead(t *testing.T) {
	w := newKidWorld(t)
	require.Equal(t, []string{w.allowedID}, idsOf(listServicesFull(t, w.env.URL, w.kidToken)))

	require.NoError(t, w.env.Store.SetKidServices(context.Background(), w.kidID, []string{w.hiddenID}))
	got := listServicesFull(t, w.env.URL, w.kidToken)
	assert.Equal(t, []string{w.hiddenID}, idsOf(got), "the new list applies at once, same session")
	cats := getCategories(t, w.env.URL, w.kidToken)
	require.Len(t, cats, 1)
	assert.Equal(t, "Secret Cat", cats[0].Name)
}

func idsOf(svcs []fullService) []string {
	out := make([]string, 0, len(svcs))
	for _, s := range svcs {
		out = append(out, s.ID)
	}
	sort.Strings(out)
	return out
}

// Hidden-marker sanity: the markers really are present for an adult, so the
// NotContains checks above are not vacuous.
func TestKid_HiddenMarkersAreRealForAdults(t *testing.T) {
	w := newKidWorld(t)
	_, svcBody := getBody(t, w.env.URL+"/api/services", "non-admin-session")
	_, statusBody := getBody(t, w.env.URL+"/api/status", "non-admin-session")
	_, catBody := getBody(t, w.env.URL+"/api/categories", "non-admin-session")
	all := string(svcBody) + string(statusBody) + string(catBody)
	for _, m := range w.hiddenMarkers() {
		assert.True(t, strings.Contains(all, m), "marker %q should be visible to an adult", m)
	}
}
