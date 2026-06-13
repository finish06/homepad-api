package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// AC A14 — Cross-user isolation (Invariant 2, D2): the headline v9 security
// invariant. With user B's token, EVERY attempt to read-mutate one of user A's
// rows — service PATCH/DELETE, icon PUT/DELETE, category PATCH/DELETE, favorite,
// layout — is answered as if the row does not exist (404, or for the idempotent
// verbs an empty/unchanged 2xx) and changes NO user-A row. 404 (never 403) so the
// existence of another user's catalog never leaks.
//
// Fixtures: admin-session and non-admin-session are two distinct users, each
// owning their OWN seeded gitea/grafana services (different ids). Here user A =
// admin-session (the victim), user B = non-admin-session (the attacker). The
// admin role is irrelevant — A14 is about ownership, not role; an admin's
// dashboard is just as isolated from a non-admin as the reverse.

// svcNameItem reads a service's id + name + icon flag — enough to prove A's row
// is byte-for-byte unchanged after B's attacks.
type svcNameItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IconLight bool   `json:"iconLight"`
}

func getServicesNamed(t *testing.T, baseURL, token string) []svcNameItem {
	t.Helper()
	resp := doJSON(t, http.MethodGet, baseURL+"/api/services", token, nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var payload struct {
		Services []svcNameItem `json:"services"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	return payload.Services
}

func TestCrossUserIsolation_A14(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	const (
		victim   = "admin-session"     // user A
		attacker = "non-admin-session" // user B
	)

	// User A's own service + a category A owns. These ids are A's; B must never
	// be able to touch them.
	aServices := getServicesNamed(t, s.URL, victim)
	require.NotEmpty(t, aServices, "victim must own at least one service")
	svcA := aServices[0]
	catA := createCategory(t, s.URL, victim, "A-Private")

	// Sanity: B owns DIFFERENT service rows than A (per-user catalog), so svcA.ID
	// is genuinely foreign to B.
	for _, b := range getServicesNamed(t, s.URL, attacker) {
		require.NotEqual(t, svcA.ID, b.ID, "per-user ids must not collide across users")
	}

	// Each attack: B aims a mutating verb at one of A's rows. Want = the status B
	// must get. For the genuinely owner-gated verbs that's 404; the idempotent
	// delete-category verb scopes silently to B's own rows (0 affected → 204) and
	// is proven harmless by the post-state assertions below.
	attacks := []struct {
		name  string
		do    func() int
		want  int
		leaks string
	}{
		{"PATCH /api/services/{A}", func() int {
			r := doJSON(t, http.MethodPatch, s.URL+"/api/services/"+svcA.ID, attacker, map[string]any{"name": "HACKED"})
			defer r.Body.Close()
			return r.StatusCode
		}, http.StatusNotFound, "another user's service must not be editable"},

		{"DELETE /api/services/{A}", func() int {
			r := doJSON(t, http.MethodDelete, s.URL+"/api/services/"+svcA.ID, attacker, nil)
			defer r.Body.Close()
			return r.StatusCode
		}, http.StatusNotFound, "another user's service must not be deletable"},

		{"PUT /api/services/{A}/icon/light", func() int {
			r := putIcon(t, s.URL, attacker, svcA.ID, "light", pngBytes(t, 32, 32))
			defer r.Body.Close()
			return r.StatusCode
		}, http.StatusNotFound, "another user's service icon must not be uploadable"},

		{"DELETE /api/services/{A}/icon/light", func() int {
			r := doJSON(t, http.MethodDelete, s.URL+"/api/services/"+svcA.ID+"/icon/light", attacker, nil)
			defer r.Body.Close()
			return r.StatusCode
		}, http.StatusNotFound, "another user's service icon must not be deletable"},

		{"PATCH /api/categories/{A}", func() int {
			r := doJSON(t, http.MethodPatch, s.URL+"/api/categories/"+catA.ID, attacker, map[string]any{"name": "HACKED"})
			defer r.Body.Close()
			return r.StatusCode
		}, http.StatusNotFound, "another user's category must not be renamable"},

		{"DELETE /api/categories/{A}", func() int {
			// Owner-scoped delete is idempotent: B's call affects 0 rows → 204,
			// but A's category must survive (asserted in the post-state below).
			r := doJSON(t, http.MethodDelete, s.URL+"/api/categories/"+catA.ID, attacker, nil)
			defer r.Body.Close()
			return r.StatusCode
		}, http.StatusNoContent, "another user's category must be untouched by B's delete"},

		{"POST /api/favorites/{A}", func() int {
			r := doJSON(t, http.MethodPost, s.URL+"/api/favorites/"+svcA.ID, attacker, nil)
			defer r.Body.Close()
			return r.StatusCode
		}, http.StatusNotFound, "B must not favorite a service it does not own"},

		{"PUT /api/layout [A's service]", func() int {
			r := doJSON(t, http.MethodPut, s.URL+"/api/layout", attacker, map[string]any{"order": []string{svcA.ID}})
			defer r.Body.Close()
			return r.StatusCode
		}, http.StatusNotFound, "B must not reference A's service in its own layout"},
	}

	for _, a := range attacks {
		t.Run(a.name, func(t *testing.T) {
			got := a.do()
			assert.Equal(t, a.want, got, a.leaks)
			// Never 403: that would confirm the row exists, leaking A's catalog
			// shape (D2). The owner-gated verbs must answer exactly 404.
			assert.NotEqual(t, http.StatusForbidden, got,
				"cross-user access must never 403 (it confirms existence) — D2")
		})
	}

	// Post-state: every one of A's rows is intact and unchanged.
	afterA := getServicesNamed(t, s.URL, victim)
	assert.Len(t, afterA, len(aServices), "A's service count is unchanged after B's attacks")
	var foundA bool
	for _, sv := range afterA {
		if sv.ID == svcA.ID {
			foundA = true
			assert.Equal(t, svcA.Name, sv.Name, "A's service name was not changed by B's PATCH")
			assert.False(t, sv.IconLight, "B's icon upload never landed on A's service")
		}
	}
	assert.True(t, foundA, "A's service still exists after B's DELETE attempt")

	// A's category survived B's PATCH + DELETE attempts.
	var foundCat bool
	for _, c := range getCategories(t, s.URL, victim) {
		if c.ID == catA.ID {
			foundCat = true
			assert.Equal(t, "A-Private", c.Name, "A's category name was not changed by B")
		}
	}
	assert.True(t, foundCat, "A's category still exists after B's attacks")
}
