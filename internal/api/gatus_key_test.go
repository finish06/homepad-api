package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-v25-gatus-key-tile-health (v25) — the Gatus endpoint slug (services.gatus_key)
// becomes part of the READ model so the TileEditModal can prefill the current key.
// The column already existed (0001_init) and PATCH already accepted it; the only
// API change is exposing it on serviceView (AC-001/012/014). Always present on the
// wire — the slug when set, "" when unmonitored — never omitted.

// serviceGK is the slice of the wire shape these tests assert on.
type serviceGK struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	GatusKey string `json:"gatus_key"`
}

func getServicesGK(t *testing.T, baseURL, token string) map[string]serviceGK {
	t.Helper()
	resp := doJSON(t, http.MethodGet, baseURL+"/api/services", token, nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var payload struct {
		Services []serviceGK `json:"services"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	out := map[string]serviceGK{}
	for _, s := range payload.Services {
		out[s.Slug] = s
	}
	return out
}

// AC-001/AC-014 — GET /api/services returns gatus_key on every service: the slug
// for a monitored tile (seeded "gitea" = "core_gitea"), "" for an unmonitored one.
// The field is always present, never omitted.
func TestGatusKey_ListExposesKeyForMonitoredAndUnmonitored(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	// An admin adds a service with NO gatus_key (unmonitored).
	resp := doJSON(t, http.MethodPost, s.URL+"/api/services", "admin-session", map[string]any{
		"slug": "proxmox", "name": "Proxmox", "url": "https://proxmox.example.com",
	})
	resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	svcs := getServicesGK(t, s.URL, "admin-session")
	require.Contains(t, svcs, "gitea", "expected the seeded gitea service")
	require.Contains(t, svcs, "proxmox", "expected the created proxmox service")

	assert.Equal(t, "core_gitea", svcs["gitea"].GatusKey,
		"a monitored tile must return its gatus_key slug (AC-001)")
	assert.Equal(t, "", svcs["proxmox"].GatusKey,
		"an unmonitored tile must return an empty gatus_key, never omitted (AC-001)")
}

// AC-001 — gatus_key is ALWAYS present on the wire (never absent), distinct from a
// JSON null or a missing key, for both monitored and unmonitored services.
func TestGatusKey_FieldAlwaysPresentOnWire(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	resp := doJSON(t, http.MethodGet, s.URL+"/api/services", "admin-session", nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var payload struct {
		Services []map[string]json.RawMessage `json:"services"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	require.NotEmpty(t, payload.Services, "expected seeded services in the catalog")

	for _, svc := range payload.Services {
		raw, ok := svc["gatus_key"]
		require.True(t, ok, "each service must carry a gatus_key field (AC-001)")
		assert.NotEqual(t, "null", string(raw),
			"gatus_key must be a string on the wire, never null; got %s", raw)
	}
}

// AC-006/AC-007/AC-012 — PATCH sets the slug, a subsequent GET returns it; PATCH
// with "" clears it, a subsequent GET returns "". The read model round-trips the
// three-state semantics the storage layer already implements.
func TestGatusKey_PatchSetAndClearRoundTrips(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	// A fresh unmonitored service to mutate.
	cResp := doJSON(t, http.MethodPost, s.URL+"/api/services", "admin-session", map[string]any{
		"slug": "gk-roundtrip", "name": "GK Roundtrip", "url": "https://gk.example.com",
	})
	var created serviceGK
	require.NoError(t, json.NewDecoder(cResp.Body).Decode(&created))
	cResp.Body.Close()

	// Set the slug.
	pResp := doJSON(t, http.MethodPatch, s.URL+"/api/services/"+created.ID, "admin-session",
		map[string]any{"gatus_key": "kube_plex"})
	pResp.Body.Close()
	require.Equal(t, http.StatusOK, pResp.StatusCode, "PATCH gatus_key must return 200")

	got := getServicesGK(t, s.URL, "admin-session")["gk-roundtrip"]
	assert.Equal(t, "kube_plex", got.GatusKey, "the set slug must persist across a re-read (AC-006)")

	// Clear the slug.
	cl := doJSON(t, http.MethodPatch, s.URL+"/api/services/"+created.ID, "admin-session",
		map[string]any{"gatus_key": ""})
	cl.Body.Close()
	require.Equal(t, http.StatusOK, cl.StatusCode, "PATCH gatus_key:\"\" must return 200")

	got = getServicesGK(t, s.URL, "admin-session")["gk-roundtrip"]
	assert.Equal(t, "", got.GatusKey, "clearing the slug must revert gatus_key to \"\" (AC-007)")
}
