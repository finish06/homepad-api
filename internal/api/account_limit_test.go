package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-household-basics scope 4 (#88) — HOMEPAD_MAX_ACCOUNTS caps all
// accounts (admin, member, kid) plus pending, unexpired invites.

// AC-028 — unset, empty or 0 is no limit; a non-negative whole number is the
// cap; anything else is an error that names the variable.
func TestParseMaxAccounts(t *testing.T) {
	for raw, want := range map[string]int{"": 0, "  ": 0, "0": 0, "12": 12, " 3 ": 3, "1": 1} {
		got, err := api.ParseMaxAccounts(raw)
		require.NoError(t, err, "HOMEPAD_MAX_ACCOUNTS=%q", raw)
		assert.Equal(t, want, got, "HOMEPAD_MAX_ACCOUNTS=%q", raw)
	}
	for _, raw := range []string{"-1", "twelve", "1.5", "12abc", "0x10", "1e3", "99999999999999999999"} {
		_, err := api.ParseMaxAccounts(raw)
		require.Error(t, err, "HOMEPAD_MAX_ACCOUNTS=%q must stop the API", raw)
		assert.Contains(t, err.Error(), "HOMEPAD_MAX_ACCOUNTS", "the message names the variable")
		assert.Contains(t, err.Error(), raw, "the message quotes the bad value")
	}
}

// limitEnv is the NewEnv fixture (three adults) with a cap.
func limitEnv(t *testing.T, limit int) *testsupport.Env {
	t.Helper()
	return testsupport.NewEnvWith(t, testsupport.Options{MaxAccounts: limit, InviteLimit: roomyLimit})
}

func isLimitBody(body []byte, limit int) bool {
	return strings.HasPrefix(string(body), fmt.Sprintf("account limit reached (limit %d)", limit))
}

// AC-028 — the count is every account plus pending, unexpired invites. Used,
// revoked and expired invites stop counting.
func TestAccountLimit_CountsAccountsAndPendingInvites(t *testing.T) {
	env := limitEnv(t, 5) // 3 adults in the fixture
	inv := mustInvite(t, env, "Alex", "user")
	code, _ := createKid(t, env, map[string]any{"name": "Sam"})
	require.Equal(t, http.StatusCreated, code, "3 adults + 1 invite + 1 kid = 5")

	code, _, b := createInvite(t, env, "admin-session", map[string]any{"name": "Blair", "role": "user"})
	assert.Equal(t, http.StatusConflict, code, "at 5 of 5: %s", b)

	require.Equal(t, http.StatusNoContent, revokeInvite(t, env, "admin-session", inv.ID))
	code, again, b := createInvite(t, env, "admin-session", map[string]any{"name": "Blair", "role": "user"})
	require.Equal(t, http.StatusCreated, code, "a revoked invite stops counting: %s", b)

	expireInvite(t, env, again.ID)
	code, _ = createKid(t, env, map[string]any{"name": "Max"})
	assert.Equal(t, http.StatusCreated, code, "an expired invite stops counting")
}

// AC-029 — at the cap, kid create, invite create, open self-registration and
// OIDC auto-creation are 409 "account limit reached", and nothing is created.
func TestAccountLimit_RefusesAtCap(t *testing.T) {
	t.Run("POST /api/admin/kids", func(t *testing.T) {
		env := limitEnv(t, 3)
		before := countUsers(t, env)
		r := doJSON(t, http.MethodPost, env.URL+"/api/admin/kids", "admin-session", map[string]any{"name": "Sam"})
		b := readAll(t, r)
		assert.Equal(t, http.StatusConflict, r.StatusCode, string(b))
		assert.True(t, isLimitBody(b, 3), "body %q", b)
		assert.Equal(t, before, countUsers(t, env), "no kid was created")
	})
	t.Run("POST /api/admin/invites", func(t *testing.T) {
		env := limitEnv(t, 3)
		code, _, b := createInvite(t, env, "admin-session", map[string]any{"name": "Alex", "role": "user"})
		assert.Equal(t, http.StatusConflict, code, string(b))
		assert.True(t, isLimitBody(b, 3), "body %q", b)
		_, list, _ := listInvites(t, env, "admin-session")
		assert.Empty(t, list, "no invite was created")
	})
	t.Run("POST /api/register in open mode", func(t *testing.T) {
		env := limitEnv(t, 3) // the fixture is open
		before := countUsers(t, env)
		r := postJSON(t, env.URL+"/api/register", map[string]string{"email": "late@example.com", "password": "a long enough password"})
		b := readAll(t, r)
		assert.Equal(t, http.StatusConflict, r.StatusCode, string(b))
		assert.True(t, isLimitBody(b, 3), "body %q", b)
		assert.Equal(t, before, countUsers(t, env))
	})
	t.Run("under the cap still works", func(t *testing.T) {
		env := limitEnv(t, 4)
		code, _ := createKid(t, env, map[string]any{"name": "Sam"})
		assert.Equal(t, http.StatusCreated, code, "3 of 4 → a kid fits")
	})
}

// AC-029 — the account cap on OIDC auto-creation. Linking an existing account
// is not a new account and still works at the cap.
func TestAccountLimit_OIDCAutoCreate(t *testing.T) {
	idp := newMockIdP(t, "new@example.com", nil)
	homepad, store := testsupport.NewOIDCServerWith(t, idp.config(""), testsupport.Options{MaxAccounts: 1})
	ctx := t.Context()
	_, err := store.CreateUser(ctx, "owner@example.com", "x", "admin")
	require.NoError(t, err)

	code := runOIDCLoginStatus(t, homepad, idp)
	assert.Equal(t, http.StatusConflict, code, "a new OIDC identity at the cap is refused")
	_, err = store.UserByEmail(ctx, "new@example.com")
	assert.Error(t, err, "no account was created")

	idp.email = "owner@example.com"
	cookie := runOIDCLogin(t, homepad, idp)
	email, _ := fetchMe(t, homepad, cookie)
	assert.Equal(t, "owner@example.com", email, "linking an existing account is not refused")
}

// AC-029 — accepting an invite that already exists, and creating the first
// account, are never refused.
func TestAccountLimit_AcceptAndFirstAccountNeverRefused(t *testing.T) {
	t.Run("accept at the cap", func(t *testing.T) {
		env := limitEnv(t, 4)
		inv := mustInvite(t, env, "Alex", "user") // 4 of 4
		code, _ := createKid(t, env, map[string]any{"name": "Sam"})
		require.Equal(t, http.StatusConflict, code, "the invite holds the last place")
		r, b := acceptInvite(t, env, acceptBody(inv.Token, "alex@example.com"))
		assert.Equal(t, http.StatusCreated, r.StatusCode, "the reserved place is used: %s", b)
	})
	t.Run("first account with a cap of 1", func(t *testing.T) {
		env := testsupport.NewEnvWith(t, testsupport.Options{Empty: true, MaxAccounts: 1, Registration: modePtr(api.RegistrationOpen)})
		r := postJSON(t, env.URL+"/api/register", map[string]string{"email": "first@example.com", "password": "a long enough password"})
		b := readAll(t, r)
		assert.Equal(t, http.StatusCreated, r.StatusCode, string(b))
		r = postJSON(t, env.URL+"/api/register", map[string]string{"email": "second@example.com", "password": "a long enough password"})
		b = readAll(t, r)
		assert.Equal(t, http.StatusConflict, r.StatusCode, "the second, in open mode, hits the cap: %s", b)
	})
}

// AC-018 / §3.5 — a refusal at the cap writes the accounts log line.
func TestAccountLimit_LogsRefusal(t *testing.T) {
	logs := captureLog(t)
	env := limitEnv(t, 3)
	code, _ := createKid(t, env, map[string]any{"name": "Sam"})
	require.Equal(t, http.StatusConflict, code)
	assert.Contains(t, logs.String(), "accounts: limit 3 reached, refused POST /api/admin/kids")
}

// AC-030 — HOMEPAD_MAX_ACCOUNTS is on the env-config allowlist.
func TestEnvConfig_AllowlistsMaxAccounts(t *testing.T) {
	t.Setenv("HOMEPAD_MAX_ACCOUNTS", "12")
	s := testsupport.NewServer(t)
	resp, body := getEnvConfig(t, s.URL, "admin-session")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var entries []envConfigEntry
	require.NoError(t, json.Unmarshal([]byte(body), &entries))
	got := map[string]string{}
	for _, e := range entries {
		got[e.Key] = e.Value
	}
	assert.Equal(t, "12", got["HOMEPAD_MAX_ACCOUNTS"])
}
