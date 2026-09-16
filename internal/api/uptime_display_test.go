package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// homepad cap6 v2 §9b — "show uptime display" moves from the global admin System
// setting to a per-USER preference. Same contract shape as themePref /
// densityPref / showHealthBar: surfaced on GET /api/me, written via PATCH
// /api/me, type-checked, session-gated, own row only.
//
// Reuses patchMeJSON/getMeRaw from density_test.go (same package).

func TestShowUptimeDisplayDefaultsToVisible(t *testing.T) {
	// The seeded fixtures have no system_settings row, so D7's default-ON applies.
	s := testsupport.NewServer(t)
	defer s.Close()
	code, me := getMeRaw(t, s.URL, "any-user")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, me["showUptimeDisplay"])
}

func TestShowUptimeDisplayPersistsAcrossSessions(t *testing.T) {
	// AC-019 — follows the account, not the session.
	s := testsupport.NewServer(t)
	defer s.Close()
	code, out := patchMeJSON(t, s.URL, "session-one", map[string]any{"showUptimeDisplay": false})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, false, out["showUptimeDisplay"])
	assert.Equal(t, "system", out["themePref"], "an untouched field keeps its value")

	_, me := getMeRaw(t, s.URL, "session-two")
	assert.Equal(t, false, me["showUptimeDisplay"])
}

func TestShowUptimeDisplayAloneIsAccepted(t *testing.T) {
	// The single-field body. The PATCH guard rejects a body carrying none of the
	// known fields, so this field had to be added to it — otherwise every write
	// from the frontend 400s and the client's rollback disguises it as a flaky
	// server. Same bug that was fixed for showHealthBar in #68.
	s := testsupport.NewServer(t)
	defer s.Close()
	code, out := patchMeJSON(t, s.URL, "any-user", map[string]any{"showUptimeDisplay": false})
	require.Equal(t, http.StatusOK, code, "showUptimeDisplay on its own must be a valid PATCH body")
	assert.Equal(t, false, out["showUptimeDisplay"])
}

func TestShowUptimeDisplayRejectsNonBoolean(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	code, _ := patchMeJSON(t, s.URL, "any-user", map[string]any{"showUptimeDisplay": "nope"})
	assert.Equal(t, http.StatusBadRequest, code)
	_, me := getMeRaw(t, s.URL, "any-user")
	assert.Equal(t, true, me["showUptimeDisplay"], "a rejected value must leave the stored one alone")
}

func TestShowUptimeDisplayRequiresSession(t *testing.T) {
	// AC-018 — session-gated.
	s := testsupport.NewServer(t)
	defer s.Close()
	code, _ := patchMeJSON(t, s.URL, "", map[string]any{"showUptimeDisplay": false})
	assert.Equal(t, http.StatusUnauthorized, code)
}

func TestShowUptimeDisplayWritesOnlyCurrentUser(t *testing.T) {
	// AC-016 — one user hiding the sparklines must not touch another's dashboard.
	// This is the whole point of the v1 -> v2 reversal.
	s := testsupport.NewServer(t)
	defer s.Close()
	code, _ := patchMeJSON(t, s.URL, "non-admin-session", map[string]any{"showUptimeDisplay": false})
	require.Equal(t, http.StatusOK, code)
	_, admin := getMeRaw(t, s.URL, "admin-session")
	assert.Equal(t, true, admin["showUptimeDisplay"], "another user's row must be untouched")
}

func TestPatchMeOtherFieldsDoNotDisturbUptimeDisplay(t *testing.T) {
	// Four fields now share this handler; writing one must not reset the others.
	s := testsupport.NewServer(t)
	defer s.Close()
	_, _ = patchMeJSON(t, s.URL, "any-user", map[string]any{"showUptimeDisplay": false})
	code, out := patchMeJSON(t, s.URL, "any-user", map[string]any{"showHealthBar": false})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, false, out["showHealthBar"])
	assert.Equal(t, false, out["showUptimeDisplay"], "a showHealthBar write must not reset the uptime preference")
	assert.Equal(t, "system", out["themePref"])
}
