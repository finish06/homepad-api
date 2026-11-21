package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-iframe-sandbox-optin — services.iframe_same_origin, the per-app opt-in
// "Let this app keep its sign-in". The overlay adds allow-same-origin to its
// sandbox only when this is on. It is on the read model as iframeSameOrigin,
// and only admins write it (classCatalogWrite).

// servicesRaw lists GET /api/services as raw objects, so a test can tell an
// absent key from a false one.
func servicesRaw(t *testing.T, baseURL, token string) map[string]map[string]any {
	t.Helper()
	code, body := getBody(t, baseURL+"/api/services", token)
	require.Equal(t, http.StatusOK, code)
	var payload struct {
		Services []map[string]any `json:"services"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	out := map[string]map[string]any{}
	for _, s := range payload.Services {
		out[s["id"].(string)] = s
	}
	return out
}

// sendRaw sends body (any JSON value) and returns the status and decoded object.
func sendRaw(t *testing.T, method, url, token string, body any) (int, map[string]any) {
	t.Helper()
	resp := doJSON(t, method, url, token, body)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

// AC-007 — every app carries iframeSameOrigin, false unless set.
func TestIframeSameOrigin_EveryAppCarriesIt(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	svcs := servicesRaw(t, s.URL, "admin-session")
	require.NotEmpty(t, svcs)
	for id, sv := range svcs {
		v, ok := sv["iframeSameOrigin"]
		require.True(t, ok, "service %s has no iframeSameOrigin", id)
		assert.Equal(t, false, v, "service %s", id)
	}
}

// AC-009, AC-012 — POST accepts it; without it a new app starts at false,
// even on Inline overlay (AC-008: new apps start strict).
func TestIframeSameOrigin_Create(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	code, plain := sendRaw(t, http.MethodPost, s.URL+"/api/services", "admin-session", map[string]any{
		"slug": "iso-plain", "name": "Plain", "url": "https://plain.example.com", "clickAction": "iframe",
	})
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, false, plain["iframeSameOrigin"], "a new overlay app starts strict")

	code, on := sendRaw(t, http.MethodPost, s.URL+"/api/services", "admin-session", map[string]any{
		"slug": "iso-on", "name": "On", "url": "https://grafana.example.com", "clickAction": "iframe",
		"iframeSameOrigin": true,
	})
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, true, on["iframeSameOrigin"])

	svcs := servicesRaw(t, s.URL, "admin-session")
	assert.Equal(t, false, svcs[plain["id"].(string)]["iframeSameOrigin"])
	assert.Equal(t, true, svcs[on["id"].(string)]["iframeSameOrigin"])
}

// AC-009 — PATCH sets it, and a PATCH without it leaves it alone.
func TestIframeSameOrigin_Patch(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	id := firstServiceID(t, s.URL, "admin-session")
	url := s.URL + "/api/services/" + id

	code, out := sendRaw(t, http.MethodPatch, url, "admin-session", map[string]any{"iframeSameOrigin": true})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, out["iframeSameOrigin"], "the PATCH response carries the saved value")
	assert.Equal(t, true, servicesRaw(t, s.URL, "admin-session")[id]["iframeSameOrigin"])

	code, out = sendRaw(t, http.MethodPatch, url, "admin-session", map[string]any{"name": "Renamed", "clickAction": "iframe"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, out["iframeSameOrigin"], "a PATCH without it leaves it unchanged")
	assert.Equal(t, true, servicesRaw(t, s.URL, "admin-session")[id]["iframeSameOrigin"])

	code, out = sendRaw(t, http.MethodPatch, url, "admin-session", map[string]any{"iframeSameOrigin": false})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, false, out["iframeSameOrigin"])
	assert.Equal(t, false, servicesRaw(t, s.URL, "admin-session")[id]["iframeSameOrigin"])
}

// AC-009 — any value that isn't a boolean is a 400, on POST and PATCH, and
// nothing is written.
func TestIframeSameOrigin_RejectsNonBooleans(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	id := firstServiceID(t, s.URL, "admin-session")

	for name, bad := range map[string]any{
		"string": "true", "number": 1, "null": nil, "object": map[string]any{}, "array": []any{true},
	} {
		t.Run(name, func(t *testing.T) {
			code, _ := sendRaw(t, http.MethodPatch, s.URL+"/api/services/"+id, "admin-session",
				map[string]any{"iframeSameOrigin": bad})
			assert.Equal(t, http.StatusBadRequest, code, "PATCH %v", bad)

			code, _ = sendRaw(t, http.MethodPost, s.URL+"/api/services", "admin-session", map[string]any{
				"slug": "iso-bad-" + name, "name": "Bad", "url": "https://bad.example.com", "iframeSameOrigin": bad,
			})
			assert.Equal(t, http.StatusBadRequest, code, "POST %v", bad)
		})
	}
	assert.Equal(t, false, servicesRaw(t, s.URL, "admin-session")[id]["iframeSameOrigin"])
	for _, sv := range servicesRaw(t, s.URL, "admin-session") {
		assert.NotContains(t, sv["slug"], "iso-bad-", "no app was created")
	}
}

// AC-010, TC-005 — only admins write it. A member or a kid gets 403, and the
// value doesn't change. A kid still reads it for an allowed app.
func TestIframeSameOrigin_OnlyAdminsWriteIt(t *testing.T) {
	env := testsupport.NewEnv(t)
	id := env.SeedService(t, "iso-kid", "Kid app", "")
	_, kid := env.SeedKid(t, "Maya", id)

	for _, token := range []string{"non-admin-session", kid} {
		code, _ := sendRaw(t, http.MethodPatch, env.URL+"/api/services/"+id, token, map[string]any{"iframeSameOrigin": true})
		assert.Equal(t, http.StatusForbidden, code)
		code, _ = sendRaw(t, http.MethodPost, env.URL+"/api/services", token, map[string]any{
			"slug": "iso-sneak", "name": "Sneak", "url": "https://sneak.example.com", "iframeSameOrigin": true,
		})
		assert.Equal(t, http.StatusForbidden, code)
	}
	assert.Equal(t, false, servicesRaw(t, env.URL, "admin-session")[id]["iframeSameOrigin"])

	code, _ := sendRaw(t, http.MethodPatch, env.URL+"/api/services/"+id, "admin-session",
		map[string]any{"clickAction": "iframe", "iframeSameOrigin": true})
	require.Equal(t, http.StatusOK, code)
	kidView := servicesRaw(t, env.URL, kid)
	require.Contains(t, kidView, id)
	assert.Equal(t, true, kidView[id]["iframeSameOrigin"], "the kid's overlay uses the app's setting")
}
