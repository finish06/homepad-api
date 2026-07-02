package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// A14 revisited under SPEC-245-224. The v9 headline invariant — a per-user
// CATALOG that user B could never read or mutate for user A — is SUPERSEDED:
// categories and services are now a single shared, admin-managed set. Shared
// reads (every user sees the same rows) and admin-only writes (a non-admin write
// is 403) are pinned in shared_catalog_test.go.
//
// What SURVIVES is per-user PERSONALIZATION layered over that shared catalog:
// favorites (AC-013) and collapse state (AC-015) are still private to each user.
// This test pins that surviving isolation — user A's favorite and collapse
// choices never leak into user B's view of the same shared rows.
//
// Fixtures: userA = admin-session (the shared-catalog owner); userB =
// non-admin-session (a plain user viewing the same shared catalog).
func TestCrossUserIsolation_A14(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	const (
		userA = "admin-session"
		userB = "non-admin-session"
	)

	// Both users read the SAME shared services (the admin's set).
	shared := listServices(t, s.URL, userA)
	require.NotEmpty(t, shared, "the shared catalog has seeded services")
	svcID := shared[0].ID
	require.ElementsMatch(t, serviceIDs(shared), serviceIDs(listServices(t, s.URL, userB)),
		"A and B see the same shared service rows")

	// Favorites stay per-user: A favorites a shared service; B, viewing the SAME
	// shared row, does not inherit that favorite.
	fav := doJSON(t, http.MethodPost, s.URL+"/api/favorites/"+svcID, userA, nil)
	fav.Body.Close()
	require.Equal(t, http.StatusNoContent, fav.StatusCode, "A favorites a shared service → 204")

	favOf := func(token string) (bool, bool) {
		for _, sv := range listServices(t, s.URL, token) {
			if sv.ID == svcID {
				return sv.Favorite, true
			}
		}
		return false, false
	}
	aFav, aok := favOf(userA)
	bFav, bok := favOf(userB)
	require.True(t, aok && bok, "both users see the shared service row")
	assert.True(t, aFav, "A sees its own favorite")
	assert.False(t, bFav, "A's favorite must NOT leak into B's view of the shared row (AC-013)")

	// Collapse stays per-user: A collapses a shared category; B's collapse set is
	// unaffected.
	cat := createCategory(t, s.URL, userA, "Shared Media")
	put := putCollapsed(t, s.URL, userA, []string{cat.ID}, true)
	put.Body.Close()
	require.Equal(t, http.StatusNoContent, put.StatusCode, "A collapses a shared category → 204")

	codeA, idsA := getCollapsed(t, s.URL, userA)
	require.Equal(t, http.StatusOK, codeA)
	assert.Equal(t, []string{cat.ID}, idsA, "A's collapse choice is recorded")

	codeB, idsB := getCollapsed(t, s.URL, userB)
	require.Equal(t, http.StatusOK, codeB)
	assert.Empty(t, idsB, "A's collapse must NOT leak into B's set (AC-015)")
}
