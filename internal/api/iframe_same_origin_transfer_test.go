package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-iframe-sandbox-optin AC-011, AC-012, TC-006 — backups carry each app's
// iframeSameOrigin, and restore and Homepage import handle it, against a real
// Postgres.

// optInBySlug reads iframe_same_origin per slug straight from the database.
func optInBySlug(t *testing.T, env *testsupport.Env) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, sv := range servicesRaw(t, env.URL, "admin-session") {
		out[sv["slug"].(string)] = sv["iframeSameOrigin"].(bool)
	}
	return out
}

func patchService(t *testing.T, env *testsupport.Env, id string, body map[string]any) {
	t.Helper()
	r := doJSON(t, http.MethodPatch, env.URL+"/api/services/"+id, "admin-session", body)
	r.Body.Close()
	require.Equal(t, http.StatusOK, r.StatusCode)
}

func restore(t *testing.T, env *testsupport.Env, mode string, file []byte) {
	t.Helper()
	p := previewRestore(t, env.URL, "admin-session", mode, file)
	code, b := applyRestore(t, env.URL, "admin-session", mode, p.PlanHash, file)
	require.Equal(t, http.StatusOK, code, "%s", b)
}

// TC-006, AC-011 — export, change the setting, restore: each app's setting
// comes back, in both modes.
func TestIframeSameOrigin_BackupRoundTrip(t *testing.T) {
	env := testsupport.NewEnv(t)
	emptyCatalog(t, env)
	on := env.SeedService(t, "grafana", "Grafana", "")
	off := env.SeedService(t, "notes", "Notes", "")
	patchService(t, env, on, map[string]any{"clickAction": "iframe", "iframeSameOrigin": true})
	patchService(t, env, off, map[string]any{"clickAction": "iframe"})

	resp, file := getExport(t, env.URL, "admin-session", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var f struct {
		Apps []map[string]any `json:"apps"`
	}
	require.NoError(t, json.Unmarshal(file, &f))
	got := map[string]any{}
	for _, a := range f.Apps {
		got[a["slug"].(string)] = a["iframeSameOrigin"]
	}
	assert.Equal(t, map[string]any{"grafana": true, "notes": false}, got, "every app carries the field")

	for _, mode := range []string{"merge", "replace"} {
		t.Run(mode, func(t *testing.T) {
			for _, sv := range servicesRaw(t, env.URL, "admin-session") {
				id := sv["id"].(string)
				patchService(t, env, id, map[string]any{"iframeSameOrigin": sv["slug"] == "notes"})
			}
			require.Equal(t, map[string]bool{"grafana": false, "notes": true}, optInBySlug(t, env))
			restore(t, env, mode, file)
			assert.Equal(t, map[string]bool{"grafana": true, "notes": false}, optInBySlug(t, env))
		})
	}
}

// AC-011, TC-006 — a 17.1.0 file has no field. Merge leaves an existing app's
// value alone and new apps get false. Replace makes every app new, so every
// app is false.
func TestIframeSameOrigin_RestoreAFileWithoutTheField(t *testing.T) {
	env := testsupport.NewEnv(t)
	emptyCatalog(t, env)
	sonarr := env.SeedService(t, "sonarr", "Sonarr", "")
	patchService(t, env, sonarr, map[string]any{"clickAction": "iframe", "iframeSameOrigin": true})
	file := fixtureBytes(t, "backups/v1.json")

	restore(t, env, "merge", file)
	assert.Equal(t, map[string]bool{"grafana": false, "jellyfin": false, "router": false, "sonarr": true, "unifi": false},
		optInBySlug(t, env), "Merge keeps sonarr's value; the new apps are off")

	again := previewRestore(t, env.URL, "admin-session", "merge", file)
	assert.True(t, again.noChanges(), "a missing field is not a change: %+v", again.Summary)

	restore(t, env, "replace", file)
	for slug, v := range optInBySlug(t, env) {
		assert.False(t, v, "%s: Replace creates every app, and a new app is off", slug)
	}
}

// AC-012 — a Homepage import creates apps with the opt-in off.
func TestIframeSameOrigin_HomepageImportStartsOff(t *testing.T) {
	env := testsupport.NewEnv(t)
	emptyCatalog(t, env)
	imp := homepageBody(t, string(fixtureBytes(t, "homepage/services.yaml")), "", "")
	p := previewImport(t, env.URL, "admin-session", imp)
	require.NotZero(t, p.Summary.Create["apps"])
	code, b := applyImport(t, env.URL, "admin-session", p.PlanHash, imp)
	require.Equal(t, http.StatusOK, code, "%s", b)

	got := optInBySlug(t, env)
	require.NotEmpty(t, got)
	for slug, v := range got {
		assert.False(t, v, slug)
	}
}
