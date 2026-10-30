package api_test

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-kid-accounts AC-007 / D3 — a kid can change favorites and theme, and
// nothing else: every catalog, category, library, layout, icon-write, admin
// and settings mutation is 403 for a kid session.

// TC-004 — "Kid can't escalate", exactly as the spec writes it.
func TestTC004_KidCannotEscalate(t *testing.T) {
	w := newKidWorld(t)

	calls := []struct {
		name, method, path string
		body               any
		want               int
	}{
		{"POST /api/services", http.MethodPost, "/api/services", map[string]any{"slug": "x", "name": "X", "url": "https://x.example.com"}, http.StatusForbidden},
		{"PATCH /api/services/{id}", http.MethodPatch, "/api/services/" + w.allowedID, map[string]any{"name": "Hacked"}, http.StatusForbidden},
		{"GET /api/library", http.MethodGet, "/api/library", nil, http.StatusForbidden},
		{"PATCH /api/admin/settings", http.MethodPatch, "/api/admin/settings", map[string]any{"showUptimeDisplay": false}, http.StatusForbidden},
		{"PATCH /api/me densityPref", http.MethodPatch, "/api/me", map[string]any{"densityPref": "list"}, http.StatusForbidden},
		{"GET hidden icon", http.MethodGet, "/api/services/" + w.hiddenID + "/icon/light", nil, http.StatusNotFound},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			r := doJSON(t, c.method, w.env.URL+c.path, w.kidToken, c.body)
			defer r.Body.Close()
			assert.Equal(t, c.want, r.StatusCode)
		})
	}

	// Nothing changed.
	for _, sv := range listServicesFull(t, w.env.URL, "admin-session") {
		assert.NotEqual(t, "Hacked", sv.Name)
		assert.NotEqual(t, "x", sv.Slug)
	}
	code, me := getMeRaw(t, w.env.URL, w.kidToken)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "large", me["densityPref"], "the rejected PATCH wrote nothing")
	cfg := getSysCfg(t, w.env.URL)
	assert.Equal(t, true, cfg["showUptimeDisplay"], "the rejected settings PATCH wrote nothing")
}

// AC-007 — the full write lock, one call per mutating route a kid could aim at.
// The route-coverage test (routes_test.go) proves this list is every such route.
func TestKid_EveryMutationIsForbidden(t *testing.T) {
	w := newKidWorld(t)
	offer := createOffer(t, w.env.URL, "admin-session", "Plex", "https://plex.example.com")
	png := pngBytes(t, 32, 32)

	calls := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/services", map[string]any{"slug": "x", "name": "X", "url": "https://x.example.com"}},
		{http.MethodPatch, "/api/services/" + w.allowedID, map[string]any{"name": "Hacked"}},
		{http.MethodDelete, "/api/services/" + w.allowedID, nil},
		{http.MethodPost, "/api/categories", map[string]any{"name": "Hacked"}},
		{http.MethodPut, "/api/categories/order", map[string]any{"order": []string{w.kidMediaCat}}},
		{http.MethodPatch, "/api/categories/" + w.kidMediaCat, map[string]any{"name": "Hacked"}},
		{http.MethodDelete, "/api/categories/" + w.kidMediaCat, nil},
		{http.MethodPut, "/api/services/" + w.allowedID + "/icon/light", png},
		{http.MethodDelete, "/api/services/" + w.allowedID + "/icon/light", nil},
		{http.MethodPost, "/api/services/" + w.allowedID + "/fetch-icon", nil},
		{http.MethodGet, "/api/library", nil},
		{http.MethodPost, "/api/library", map[string]any{"name": "Hacked", "url": "https://h.example.com"}},
		{http.MethodPut, "/api/library/order", map[string]any{"order": []string{offer.ID}}},
		{http.MethodPatch, "/api/library/" + offer.ID, map[string]any{"name": "Hacked"}},
		{http.MethodDelete, "/api/library/" + offer.ID, nil},
		{http.MethodPost, "/api/library/" + offer.ID + "/add", nil},
		{http.MethodPut, "/api/layout", map[string]any{"order": []string{w.allowedID}}},
		{http.MethodPut, "/api/me/collapsed-categories", map[string]any{"collapsed": []string{w.kidMediaCat}}},
		{http.MethodPost, "/api/status/refresh", nil},
		{http.MethodPatch, "/api/admin/settings", map[string]any{"statusDegradedMs": 1}},
		{http.MethodGet, "/api/admin/env-config", nil},
	}
	for _, c := range calls {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			var r *http.Response
			if b, ok := c.body.([]byte); ok {
				r = putIcon(t, w.env.URL, w.kidToken, w.allowedID, "light", b)
			} else {
				r = doJSON(t, c.method, w.env.URL+c.path, w.kidToken, c.body)
			}
			defer r.Body.Close()
			body, _ := io.ReadAll(r.Body)
			assert.Equal(t, http.StatusForbidden, r.StatusCode, "kid %s %s must be 403", c.method, c.path)
			assertNoHiddenMarkers(t, w, c.method+" "+c.path, body)
		})
	}

	// Nothing was written: the catalog, categories, library, layout and the
	// kid's collapsed set are exactly as before.
	svcs := listServicesFull(t, w.env.URL, "admin-session")
	assert.Len(t, svcs, 4)
	for _, sv := range svcs {
		assert.NotEqual(t, "Hacked", sv.Name)
	}
	for _, c := range getCategories(t, w.env.URL, "admin-session") {
		assert.NotEqual(t, "Hacked", c.Name)
	}
	lib := getLibrary(t, w.env.URL, "admin-session")
	for _, o := range lib {
		assert.NotEqual(t, "Hacked", o.Name)
	}
	ctx := context.Background()
	for _, tbl := range []string{"user_layout", "user_collapsed_categories"} {
		var n int
		require.NoError(t, w.env.Store.Pool().QueryRow(ctx, `SELECT count(*) FROM `+tbl+` WHERE user_id = $1`, w.kidID).Scan(&n))
		assert.Zero(t, n, "%s: the kid wrote nothing", tbl)
	}
	var icons int
	require.NoError(t, w.env.Store.Pool().QueryRow(ctx, `SELECT count(*) FROM service_icons WHERE service_id = $1`, w.allowedID).Scan(&icons))
	assert.Zero(t, icons, "the kid's icon upload wrote nothing")
}

// AC-007 / D3 — PATCH /api/me for a kid accepts only themePref. Any other key,
// alone or alongside themePref, is 403 and writes nothing.
func TestKid_PatchMeOnlyThemePref(t *testing.T) {
	w := newKidWorld(t)

	code, me := patchMeJSON(t, w.env.URL, w.kidToken, map[string]any{"themePref": "dark"})
	require.Equal(t, http.StatusOK, code, "a kid may change the theme")
	assert.Equal(t, "dark", me["themePref"])

	for _, body := range []map[string]any{
		{"densityPref": "list"},
		{"showHealthBar": true},
		{"showUptimeDisplay": false},
		{"themePref": "light", "densityPref": "list"},
		{"themePref": "light", "showHealthBar": true},
		{"themePref": "light", "name": "Boss"},
		{"role": "admin"},
	} {
		code, _ := patchMeJSON(t, w.env.URL, w.kidToken, body)
		assert.Equal(t, http.StatusForbidden, code, "kid PATCH /api/me %v must be 403", body)
	}

	code, me = getMeRaw(t, w.env.URL, w.kidToken)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "dark", me["themePref"], "a rejected mixed PATCH did not half-apply")
	assert.Equal(t, "large", me["densityPref"])
	assert.Equal(t, false, me["showHealthBar"])
	assert.Equal(t, "kid", me["role"])

	// Validation still applies to the one allowed field.
	code, _ = patchMeJSON(t, w.env.URL, w.kidToken, map[string]any{"themePref": "neon"})
	assert.Equal(t, http.StatusBadRequest, code)

	// Adults are unchanged: a plain user still sets density.
	code, me = patchMeJSON(t, w.env.URL, "non-admin-session", map[string]any{"densityPref": "list"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "list", me["densityPref"])
}

// What a kid CAN still do: read its own account, sign out.
func TestKid_AllowedSessionRoutes(t *testing.T) {
	w := newKidWorld(t)
	code, _ := getMeRaw(t, w.env.URL, w.kidToken)
	assert.Equal(t, http.StatusOK, code)

	out := doJSON(t, http.MethodPost, w.env.URL+"/api/logout", w.kidToken, nil)
	out.Body.Close()
	assert.Equal(t, http.StatusNoContent, out.StatusCode, "K-2: a kid can sign out")
	code, _ = getMeRaw(t, w.env.URL, w.kidToken)
	assert.Equal(t, http.StatusUnauthorized, code, "the kid session is gone after sign-out")
}
