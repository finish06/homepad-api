package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// v5 collapsible-categories — backend slice. API integration for the
// session-gated GET/PUT /api/me/collapsed-categories surface and the FK-cascade
// / id-stability guarantees. Covers the API-verified ACs A2–A8 + A11; the
// disclosure interaction (A1/A9/A10/A12) is the web slice.

// getCollapsed reads the current user's collapsed category-id set. Returns the
// HTTP status and the decoded ids (nil unless 200).
func getCollapsed(t *testing.T, baseURL, token string) (int, []string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/api/me/collapsed-categories", nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "homepad_session", Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var payload struct {
		Collapsed []string `json:"collapsed"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	return resp.StatusCode, payload.Collapsed
}

// putCollapsed replaces the current user's collapsed set with ids.
func putCollapsed(t *testing.T, baseURL, token string, ids []string, withCookie bool) *http.Response {
	t.Helper()
	body := map[string]any{"collapsed": ids}
	if !withCookie {
		return doNoCookie(t, http.MethodPut, baseURL+"/api/me/collapsed-categories", body)
	}
	return doJSON(t, http.MethodPut, baseURL+"/api/me/collapsed-categories", token, body)
}

func doNoCookie(t *testing.T, method, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// A2 — default is expanded: a fresh user's collapsed set is empty.
func TestCollapsedDefaultsEmpty(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	code, ids := getCollapsed(t, s.URL, "any-user")
	require.Equal(t, http.StatusOK, code, "GET collapsed-categories must return 200 for a session")
	assert.Empty(t, ids, "a fresh user has nothing collapsed (everything expanded)")
}

// A3 — collapse state persists per-user across sessions: collapse a category in
// one session, read it back in a fresh session for the same user.
func TestCollapsePersistsAcrossSessions(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	media := createCategory(t, s.URL, "admin-session", "Media")

	resp := putCollapsed(t, s.URL, "session-one", []string{media.ID}, true)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "PUT collapsed-categories must return 204")

	// session-two is the same user as session-one — proves the set lives on the
	// account, not the session.
	code, ids := getCollapsed(t, s.URL, "session-two")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, []string{media.ID}, ids, "collapsed set must persist per-user across sessions")
}

// A4 — collapse state is private to the user: user A collapsing a category must
// not affect user B's set.
func TestCollapseIsPrivateToUser(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	media := createCategory(t, s.URL, "admin-session", "Media")

	// The regular user collapses it.
	resp := putCollapsed(t, s.URL, "non-admin-session", []string{media.ID}, true)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	// The admin (a different user) still sees everything expanded.
	code, ids := getCollapsed(t, s.URL, "admin-session")
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, ids, "one user's collapse must not leak into another user's set")
}

// A5 — PUT replaces the set; unknown/stale ids in the body are silently dropped
// (no 4xx). A well-formed-but-deleted id and a malformed id are both ignored.
func TestPutReplacesAndDropsUnknownIds(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	media := createCategory(t, s.URL, "admin-session", "Media")
	stale := createCategory(t, s.URL, "admin-session", "Infra")

	// Delete one category so its id is well-formed but no longer exists.
	del := doJSON(t, http.MethodDelete, s.URL+"/api/categories/"+stale.ID, "admin-session", nil)
	del.Body.Close()
	require.Equal(t, http.StatusNoContent, del.StatusCode)

	resp := putCollapsed(t, s.URL, "any-user",
		[]string{media.ID, stale.ID, "not-a-uuid"}, true)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode,
		"unknown/stale/malformed ids must be silently dropped, not 4xx")

	code, ids := getCollapsed(t, s.URL, "any-user")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, []string{media.ID}, ids, "only the live category id survives the PUT")

	// PUT is a whole-set replace: an empty body clears the set.
	clear := putCollapsed(t, s.URL, "any-user", []string{}, true)
	clear.Body.Close()
	require.Equal(t, http.StatusNoContent, clear.StatusCode)
	_, ids = getCollapsed(t, s.URL, "any-user")
	assert.Empty(t, ids, "PUT with an empty set replaces (clears) the prior set")
}

// A6 — both endpoints require a session; unauthenticated → 401.
func TestCollapseRequiresSession(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	code, _ := getCollapsed(t, s.URL, "")
	assert.Equal(t, http.StatusUnauthorized, code, "GET without a session must be 401")

	resp := putCollapsed(t, s.URL, "", []string{}, false)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "PUT without a session must be 401")
}

// A7 — deleting a category removes everyone's collapse row for it (FK cascade):
// no orphan state pointing at a category that no longer exists.
func TestDeleteCategoryCascadesCollapse(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	media := createCategory(t, s.URL, "admin-session", "Media")

	resp := putCollapsed(t, s.URL, "any-user", []string{media.ID}, true)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	del := doJSON(t, http.MethodDelete, s.URL+"/api/categories/"+media.ID, "admin-session", nil)
	del.Body.Close()
	require.Equal(t, http.StatusNoContent, del.StatusCode)

	code, ids := getCollapsed(t, s.URL, "any-user")
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, ids, "FK cascade must drop the collapse row when its category is deleted")
}

// A8 — renaming or reordering a category does not change its collapse state
// (keyed on id, not name or order).
func TestRenameReorderKeepsCollapse(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	media := createCategory(t, s.URL, "admin-session", "Media")
	infra := createCategory(t, s.URL, "admin-session", "Infra")

	resp := putCollapsed(t, s.URL, "any-user", []string{media.ID}, true)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	// Rename the collapsed category.
	ren := doJSON(t, http.MethodPatch, s.URL+"/api/categories/"+media.ID, "admin-session",
		map[string]any{"name": "Movies"})
	ren.Body.Close()
	require.Equal(t, http.StatusOK, ren.StatusCode)

	// Reorder both categories.
	ord := doJSON(t, http.MethodPut, s.URL+"/api/categories/order", "admin-session",
		map[string]any{"order": []string{infra.ID, media.ID}})
	ord.Body.Close()
	require.Equal(t, http.StatusNoContent, ord.StatusCode)

	code, ids := getCollapsed(t, s.URL, "any-user")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, []string{media.ID}, ids, "rename + reorder must not change collapse state (keyed on id)")
}

// A11 — a newly-created category renders expanded for all users automatically
// (it is in no one's collapsed set).
func TestNewCategoryRendersExpanded(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	createCategory(t, s.URL, "admin-session", "Media")

	code, ids := getCollapsed(t, s.URL, "any-user")
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, ids, "a brand-new category is expanded by default (not in the collapsed set)")
}
