package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-tile-click-action-20260710 (v23) — services.click_action: the per-tile
// click behavior (new_tab | same_tab | iframe), admin-set on the shared catalog
// row. Migration 0011 adds the column with DEFAULT 'new_tab' so every pre-existing
// tile keeps today's new-tab behavior (§3.1, AC-001). Persisted, read on
// GET /api/services, written via PATCH /api/services/{id} {clickAction}. Enum is
// validated at the API (400 on anything else).

// serviceCA is the slice of the wire shape these tests assert on.
type serviceCA struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	ClickAction string `json:"clickAction"`
}

func getServicesCA(t *testing.T, baseURL, token string) map[string]serviceCA {
	t.Helper()
	resp := doJSON(t, http.MethodGet, baseURL+"/api/services", token, nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var payload struct {
		Services []serviceCA `json:"services"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	out := map[string]serviceCA{}
	for _, s := range payload.Services {
		out[s.ID] = s
	}
	return out
}

// createServiceCA POSTs a service (optionally with clickAction) and returns the
// created row's wire form.
func createServiceCA(t *testing.T, baseURL, token string, body map[string]any) serviceCA {
	t.Helper()
	resp := doJSON(t, http.MethodPost, baseURL+"/api/services", token, body)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "POST /api/services must return 201")
	var sv serviceCA
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sv))
	return sv
}

// AC-001 — every seeded (pre-migration) service reads back as new_tab. The DB
// DEFAULT 'new_tab' covers all rows created before the column existed.
func TestClickAction_SeededServicesDefaultNewTab(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	svcs := getServicesCA(t, s.URL, "admin-session")
	require.NotEmpty(t, svcs, "expected seeded services in the catalog")
	for id, sv := range svcs {
		assert.Equal(t, "new_tab", sv.ClickAction, "seeded service %s must default to new_tab", id)
	}
}

// AC-002 — a service created without clickAction defaults to new_tab, both in the
// create response and on a subsequent read.
func TestClickAction_CreateDefaultsNewTab(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	created := createServiceCA(t, s.URL, "admin-session", map[string]any{
		"slug": "ca-newtab", "name": "CA NewTab", "url": "https://grafana.example.com",
	})
	assert.Equal(t, "new_tab", created.ClickAction, "create response must carry new_tab default")

	got := getServicesCA(t, s.URL, "admin-session")[created.ID]
	assert.Equal(t, "new_tab", got.ClickAction, "listed service must default to new_tab")
}

// §3.2 / §5 — an explicit clickAction on create is accepted and returned.
func TestClickAction_CreateAcceptsIframe(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	created := createServiceCA(t, s.URL, "admin-session", map[string]any{
		"slug": "ca-iframe", "name": "CA Iframe", "url": "https://netdata.example.com",
		"clickAction": "iframe",
	})
	assert.Equal(t, "iframe", created.ClickAction)

	got := getServicesCA(t, s.URL, "admin-session")[created.ID]
	assert.Equal(t, "iframe", got.ClickAction)
}

// AC-011 / AC-013 — an admin changes a tile's click action; it persists across a
// fresh read (and, being on the shared row, for all viewers).
func TestClickAction_PatchPersists(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	created := createServiceCA(t, s.URL, "admin-session", map[string]any{
		"slug": "ca-persist", "name": "CA Persist", "url": "https://uptime.example.com",
	})

	resp := doJSON(t, http.MethodPatch, s.URL+"/api/services/"+created.ID, "admin-session",
		map[string]any{"clickAction": "same_tab"})
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "PATCH clickAction must return 200")

	got := getServicesCA(t, s.URL, "admin-session")[created.ID]
	assert.Equal(t, "same_tab", got.ClickAction, "clickAction must persist across a re-read")
}

// #342 — a per-tile launch type changed via the edit-service PATCH must survive a
// re-fetch (a page reload). The UI's "overlay" launch type is the `iframe` enum
// value (SPEC-tile-click-action-20260710 §4.2 — the "Inline overlay" option that
// embeds the service in an overlay panel), so the reported "overlay reverts on
// reload" symptom is the `iframe` case here. This covers EVERY enum value, not
// just one, closing the issue's stated test gap. Named for the observed symptom
// (reverts on reload), not a theorized cause.
func TestClickAction_AllEnumValuesSurviveReload(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	// 'iframe' is the UI's "Inline overlay" launch type — the value in the #342 report.
	for _, want := range []string{"new_tab", "same_tab", "iframe"} {
		want := want
		t.Run(want, func(t *testing.T) {
			// A fresh service starts at the DB default 'new_tab'.
			created := createServiceCA(t, s.URL, "admin-session", map[string]any{
				"slug": "ca-rt-" + want, "name": "CA RT " + want, "url": "https://rt.example.com",
			})

			// Change the launch type via the same PATCH the edit-service form uses.
			resp := doJSON(t, http.MethodPatch, s.URL+"/api/services/"+created.ID, "admin-session",
				map[string]any{"clickAction": want})
			resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode, "PATCH clickAction=%q must return 200", want)

			// Re-fetch the catalog (what a page reload does) and assert it stuck.
			got := getServicesCA(t, s.URL, "admin-session")[created.ID]
			assert.Equal(t, want, got.ClickAction, "clickAction %q must survive a reload (re-fetch)", want)
		})
	}
}

// §3.2 — an invalid enum value is rejected with 400 on both create and update,
// and nothing is changed.
func TestClickAction_RejectsInvalidEnum(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	// create with a bogus value → 400
	resp := doJSON(t, http.MethodPost, s.URL+"/api/services", "admin-session",
		map[string]any{"slug": "ca-bad", "name": "CA Bad", "url": "https://bad.example.com", "clickAction": "popup"})
	resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "invalid clickAction on create must be 400")

	// a valid create, then a bogus PATCH → 400, value unchanged
	created := createServiceCA(t, s.URL, "admin-session", map[string]any{
		"slug": "ca-good", "name": "CA Good", "url": "https://good.example.com",
	})
	resp = doJSON(t, http.MethodPatch, s.URL+"/api/services/"+created.ID, "admin-session",
		map[string]any{"clickAction": "overlay"})
	resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "invalid clickAction on update must be 400")

	got := getServicesCA(t, s.URL, "admin-session")[created.ID]
	assert.Equal(t, "new_tab", got.ClickAction, "a rejected PATCH must leave the value unchanged")
}
