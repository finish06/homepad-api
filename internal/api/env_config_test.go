package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-v26-admin-env-config §6.3 — GET /api/admin/env-config exposes an EXPLICIT
// allowlist of non-sensitive runtime env vars to admins only. The security crux
// (§4) is that the response contains ONLY the allowlisted keys; a sensitive key
// (DATABASE_URL, OIDC_CLIENT_SECRET) is absent, not redacted. These integration
// tests drive the real HTTP surface against a live Postgres, so a missing route
// fails on the status assertion (RED) rather than a compile error.

type envConfigEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// the complete allowlist the endpoint must return, in order (AC-003).
var wantEnvConfigKeys = []string{
	"GATUS_BASE_URL",
	"COOKIE_SECURE",
	"HOMEPAD_REGISTRATION",
	"PORT",
	"OIDC_ENABLED",
	"OIDC_ISSUER",
	"OIDC_DISCOVERY_URL",
	"OIDC_REDIRECT_URL",
	"OIDC_CLIENT_ID",
	"OIDC_ADMIN_GROUP",
}

func getEnvConfig(t *testing.T, baseURL, token string) (*http.Response, string) {
	t.Helper()
	resp := doJSON(t, http.MethodGet, baseURL+"/api/admin/env-config", token, nil)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	return resp, string(raw)
}

// AC-019 — an admin session returns 200 with a JSON array of every allowlisted
// var, and GATUS_BASE_URL carries the configured value.
func TestEnvConfig_AdminReturnsAllowlist(t *testing.T) {
	t.Setenv("GATUS_BASE_URL", "http://gatus.test.local")
	s := testsupport.NewServer(t)
	defer s.Close()

	resp, body := getEnvConfig(t, s.URL, "admin-session")
	require.Equal(t, http.StatusOK, resp.StatusCode, "admin GET must return 200")

	var entries []envConfigEntry
	require.NoError(t, json.Unmarshal([]byte(body), &entries))

	// AC-003 — exactly the allowlisted keys, in order, nothing else.
	gotKeys := make([]string, len(entries))
	for i, e := range entries {
		gotKeys[i] = e.Key
	}
	assert.Equal(t, wantEnvConfigKeys, gotKeys, "response must be exactly the allowlist, in order")

	byKey := map[string]string{}
	for _, e := range entries {
		byKey[e.Key] = e.Value
	}
	assert.Equal(t, "http://gatus.test.local", byKey["GATUS_BASE_URL"], "GATUS_BASE_URL must echo the configured value")
}

// AC-006 — an unset allowlisted var is present with an empty-string value, not
// omitted from the array.
func TestEnvConfig_UnsetVarPresentAsEmpty(t *testing.T) {
	// Force one allowlisted var unset for this test.
	t.Setenv("OIDC_ISSUER", "")
	s := testsupport.NewServer(t)
	defer s.Close()

	resp, body := getEnvConfig(t, s.URL, "admin-session")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var entries []envConfigEntry
	require.NoError(t, json.Unmarshal([]byte(body), &entries))

	var found bool
	for _, e := range entries {
		if e.Key == "OIDC_ISSUER" {
			found = true
			assert.Equal(t, "", e.Value, "unset var must appear with an empty value")
		}
	}
	assert.True(t, found, "OIDC_ISSUER must be present even when unset")
}

// AC-020 — an unauthenticated caller (unbound session token) gets 401.
func TestEnvConfig_UnauthenticatedRejected(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	resp, _ := getEnvConfig(t, s.URL, "no-such-session")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "no session must return 401")
}

// AC-021 — a non-admin authenticated caller gets 403.
func TestEnvConfig_NonAdminForbidden(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	resp, _ := getEnvConfig(t, s.URL, "non-admin-session")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "non-admin must return 403")
}

// AC-004 / AC-022 — DATABASE_URL never appears in the response, neither as a key
// nor as a value. DATABASE_URL is genuinely set in this test's environment (it is
// the test DSN), so a naive "dump all env" handler would leak it here — that is
// exactly what this asserts against.
func TestEnvConfig_DatabaseURLAbsent(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	resp, body := getEnvConfig(t, s.URL, "admin-session")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.False(t, strings.Contains(body, "DATABASE_URL"), "DATABASE_URL must be absent from the response body")
}

// AC-005 / AC-023 — OIDC_CLIENT_SECRET never appears, neither as a key nor as a
// value. A sentinel secret value is set in the env; neither the key nor the
// sentinel may surface.
func TestEnvConfig_ClientSecretAbsent(t *testing.T) {
	const sentinel = "shhh-oidc-shared-secret-sentinel"
	t.Setenv("OIDC_CLIENT_SECRET", sentinel)
	s := testsupport.NewServer(t)
	defer s.Close()

	resp, body := getEnvConfig(t, s.URL, "admin-session")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.False(t, strings.Contains(body, "OIDC_CLIENT_SECRET"), "OIDC_CLIENT_SECRET key must be absent")
	assert.False(t, strings.Contains(body, sentinel), "the secret value must never leak into the response")
}
