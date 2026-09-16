package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// homepad SPEC-health-bar-visibility-toggle §7 — per-USER visibility of the
// health panel's distribution bar. Same contract shape as themePref/densityPref:
// surfaced on GET /api/me, written via PATCH /api/me, type-checked,
// session-gated, own row only.
//
// Reuses patchMeJSON/getMeRaw from density_test.go (same package).

func TestShowHealthBarDefaultsToVisible(t *testing.T) {
	// AC-001 — the column is NOT NULL DEFAULT TRUE, so a user who never chose
	// sees the bar and nobody's dashboard changes on deploy.
	s := testsupport.NewServer(t)
	defer s.Close()
	code, me := getMeRaw(t, s.URL, "any-user")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, me["showHealthBar"], "a user who never chose sees the bar")
}

func TestShowHealthBarPersistsAcrossSessions(t *testing.T) {
	// AC-005 — the preference follows the user, not the device/session.
	s := testsupport.NewServer(t)
	defer s.Close()
	code, out := patchMeJSON(t, s.URL, "session-one", map[string]any{"showHealthBar": false})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, false, out["showHealthBar"], "PATCH echoes the stored value")
	assert.Equal(t, "system", out["themePref"], "an untouched field keeps its value")

	_, me := getMeRaw(t, s.URL, "session-two")
	assert.Equal(t, false, me["showHealthBar"], "the same user on another session sees the choice")
}

func TestShowHealthBarAloneIsAccepted(t *testing.T) {
	// The single-field body. The PATCH handler rejects a body carrying none of
	// the known fields, so showHealthBar must be added to that guard — otherwise
	// every write from the frontend 400s and the client rollback (AC-010) masks
	// it as a server error rather than a missing case.
	s := testsupport.NewServer(t)
	defer s.Close()
	code, out := patchMeJSON(t, s.URL, "any-user", map[string]any{"showHealthBar": false})
	require.Equal(t, http.StatusOK, code, "showHealthBar on its own must be a valid PATCH body")
	assert.Equal(t, false, out["showHealthBar"])
}

func TestShowHealthBarRejectsNonBoolean(t *testing.T) {
	// AC-009 — an invalid value is 400 and must leave the stored value alone.
	s := testsupport.NewServer(t)
	defer s.Close()
	code, _ := patchMeJSON(t, s.URL, "any-user", map[string]any{"showHealthBar": "nope"})
	assert.Equal(t, http.StatusBadRequest, code)
	_, me := getMeRaw(t, s.URL, "any-user")
	assert.Equal(t, true, me["showHealthBar"], "a rejected value must leave the stored one alone")
}

func TestShowHealthBarRequiresSession(t *testing.T) {
	// AC-008 — no session, no write.
	s := testsupport.NewServer(t)
	defer s.Close()
	code, _ := patchMeJSON(t, s.URL, "", map[string]any{"showHealthBar": false})
	assert.Equal(t, http.StatusUnauthorized, code)
}

func TestShowHealthBarWritesOnlyCurrentUser(t *testing.T) {
	// AC-006 — per-user isolation: one user hiding the bar must not touch another.
	s := testsupport.NewServer(t)
	defer s.Close()
	code, _ := patchMeJSON(t, s.URL, "non-admin-session", map[string]any{"showHealthBar": false})
	require.Equal(t, http.StatusOK, code)
	_, admin := getMeRaw(t, s.URL, "admin-session")
	assert.Equal(t, true, admin["showHealthBar"], "another user's row must be untouched")
}

func TestPatchMeOtherFieldsDoNotDisturbHealthBar(t *testing.T) {
	// Regression guard, mirroring TestPatchMeThemeOnlyStillWorks: adding a third
	// field must not make the existing two clobber it.
	s := testsupport.NewServer(t)
	defer s.Close()
	_, _ = patchMeJSON(t, s.URL, "any-user", map[string]any{"showHealthBar": false})
	code, out := patchMeJSON(t, s.URL, "any-user", map[string]any{"themePref": "dark"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "dark", out["themePref"])
	assert.Equal(t, false, out["showHealthBar"], "themePref write must not reset the bar preference")
}
