package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/oidc"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-kid-accounts — how a kid account is (and is not) reached: GET /api/me
// reports the kid role, the session lifetime is 180 days, and neither
// self-registration (AC-013) nor password login can produce or reach a kid.

// §7 — GET /api/me gains role "kid" and displayName.
func TestKid_MeReportsKidRoleAndDisplayName(t *testing.T) {
	env := testsupport.NewEnv(t)
	_, tok := env.SeedKid(t, "Maya")

	code, me := getMeRaw(t, env.URL, tok)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "kid", me["role"])
	assert.Equal(t, "Maya", me["displayName"])
	assert.Equal(t, "Maya", me["name"], "name stays the display name, as for adults")
	assert.Equal(t, "blue", me["avatarColor"])
	assert.Equal(t, "", me["email"], "a kid has no email")
	assert.Equal(t, false, me["showHealthBar"], "AC-019")
	assert.Equal(t, "large", me["densityPref"], "UX K-3")

	// Adults gain displayName too (additive), nothing else changes.
	code, me = getMeRaw(t, env.URL, "non-admin-session")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "user", me["role"])
	assert.Equal(t, "user@homepad.test", me["email"])
	assert.Contains(t, me, "displayName")
}

// AC-010 — a kid session slides with a 180-day lifetime and the cookie is
// re-issued with that Max-Age.
func TestKid_SessionSlidesWith180DayMaxAge(t *testing.T) {
	env := testsupport.NewEnv(t)
	kidID, tok := env.SeedKid(t, "Maya")
	ctx := context.Background()

	// Age the session past the touch throttle so this request slides it.
	_, err := env.Store.Pool().Exec(ctx,
		`UPDATE sessions SET last_seen_at = now() - interval '2 minutes' WHERE user_id = $1`, kidID)
	require.NoError(t, err)

	resp := doJSON(t, http.MethodGet, env.URL+"/api/me", tok, nil)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	ck := sessionCookieFrom(resp)
	require.NotNil(t, ck, "the slide re-issues the cookie")
	assert.Equal(t, int(session.KidLifetime/time.Second), ck.MaxAge, "kid cookie Max-Age is 180 days")

	var expires time.Time
	require.NoError(t, env.Store.Pool().QueryRow(ctx,
		`SELECT expires_at FROM sessions WHERE user_id = $1`, kidID).Scan(&expires))
	assert.WithinDuration(t, time.Now().Add(session.KidLifetime), expires, time.Minute,
		"the kid session's expiry slid by 180 days")
}

// AC-013 — POST /api/register can never create a kid, whatever the body says.
func TestRegister_CannotCreateKid(t *testing.T) {
	env := testsupport.NewEnv(t)
	body, _ := json.Marshal(map[string]any{
		"email": "sneaky@example.com", "password": "correct horse battery", "role": "kid",
		"displayName": "Sneaky",
	})
	resp, err := http.Post(env.URL+"/api/register", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "existing register behaviour is unchanged")
	var u map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&u))
	assert.Equal(t, "user", u["role"], "self-registration cannot pick a role")

	var role string
	require.NoError(t, env.Store.Pool().QueryRow(context.Background(),
		`SELECT role FROM users WHERE email = 'sneaky@example.com'`).Scan(&role))
	assert.Equal(t, "user", role)
}

// The first-account-becomes-admin rule does not count kids: on an install whose
// only rows are kids, the first adult to register still bootstraps as admin.
func TestRegister_FirstAdultIsAdminEvenIfKidsExist(t *testing.T) {
	srv, store := testsupport.NewOIDCServer(t, oidc.Config{})
	_, err := store.CreateKid(context.Background(), "Orphan", "blue")
	require.NoError(t, err)

	body, _ := json.Marshal(map[string]string{"email": "first@example.com", "password": "correct horse battery"})
	resp, err := http.Post(srv.URL+"/api/register", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var u map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&u))
	assert.Equal(t, "admin", u["role"], "kids do not count toward the first-user bootstrap")
}

// A kid cannot sign in through /api/login: there is no email to sign in with,
// and an empty one is rejected before any lookup.
func TestLogin_CannotReachKid(t *testing.T) {
	env := testsupport.NewEnv(t)
	env.SeedKid(t, "Maya")
	for _, email := range []string{"", "Maya", "maya"} {
		body, _ := json.Marshal(map[string]string{"email": email, "password": "whatever-long-enough"})
		resp, err := http.Post(env.URL+"/api/login", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		resp.Body.Close()
		assert.NotEqual(t, http.StatusOK, resp.StatusCode, "login as %q must not succeed", email)
		assert.Nil(t, sessionCookieFrom(resp), "no session is issued")
	}
}
