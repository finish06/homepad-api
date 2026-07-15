package api

import (
	"net/http"
	"os"
)

// SPEC-v26-admin-env-config §4/§7.1 — the read-only admin Environment
// Configuration surface. The security crux is the EXPLICIT allowlist: the
// handler returns ONLY the keys named below, so a new env var (a future secret)
// is invisible by default and can only be surfaced by a deliberate, reviewable
// change to allowlistedEnvVars. This inverts the fragile blocklist approach —
// nothing is ever exposed by accident.

// envConfigEntry is one allowlisted runtime config variable.
type envConfigEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// allowlistedEnvVars is the COMPLETE set of env vars that may be returned by
// GET /api/admin/env-config. Everything not on this list is permanently absent
// from the response — DATABASE_URL and OIDC_CLIENT_SECRET are deliberately NOT
// here (they carry credentials, §4). Adding a var requires a change here.
var allowlistedEnvVars = []string{
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

// handleAdminEnvConfig serves the allowlisted runtime config to admins. It reads
// ONLY the fixed allowlist keys from the process environment; it never takes a
// key name from the request. An unset var is returned with an empty value
// (present, not omitted — AC-006). requireAdmin writes 401/403 itself.
func (s *server) handleAdminEnvConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	entries := make([]envConfigEntry, 0, len(allowlistedEnvVars))
	for _, k := range allowlistedEnvVars {
		entries = append(entries, envConfigEntry{Key: k, Value: os.Getenv(k)})
	}
	writeJSON(w, http.StatusOK, entries)
}
