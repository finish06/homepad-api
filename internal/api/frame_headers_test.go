package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// homepad#612 — the ingress sends /api/* straight to the API, past the web
// nginx that adds the anti-framing headers to everything it serves, so the
// API sends them itself on every response: success, error, 404, 429 and the
// non-/api routes alike.
func TestEveryResponseRefusesFraming(t *testing.T) {
	env := testsupport.NewEnvWith(t, testsupport.Options{
		Limits: api.Limits{Login: api.RateLimit{PerMinute: 1, Burst: 1}},
	})
	// Spend the login bucket, so the next login is a 429.
	code, _, _ := login(t, env, "", "user@homepad.test", "not the password")
	require.Equal(t, http.StatusUnauthorized, code)

	cases := []struct {
		name, method, path, session string
		body                        string
		want                        int
	}{
		{"public config", http.MethodGet, "/api/auth/config", "", "", http.StatusOK},
		{"system config", http.MethodGet, "/api/system/config", "", "", http.StatusOK},
		{"signed out", http.MethodGet, "/api/me", "", "", http.StatusUnauthorized},
		{"signed in", http.MethodGet, "/api/me", "admin-session", "", http.StatusOK},
		{"admin json", http.MethodGet, "/api/admin/accounts", "admin-session", "", http.StatusOK},
		{"forbidden", http.MethodGet, "/api/admin/accounts", "non-admin-session", "", http.StatusForbidden},
		{"rate limited", http.MethodPost, "/api/login", "", `{"email":"user@homepad.test","password":"x"}`, http.StatusTooManyRequests},
		{"unknown route", http.MethodGet, "/api/no-such-route", "", "", http.StatusNotFound},
		{"wrong method", http.MethodDelete, "/api/auth/config", "", "", http.StatusMethodNotAllowed},
		{"liveness", http.MethodGet, "/live", "", "", http.StatusOK},
		{"health", http.MethodGet, "/health", "", "", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(c.method, env.URL+c.path, strings.NewReader(c.body))
			require.NoError(t, err)
			if c.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			if c.session != "" {
				req.AddCookie(&http.Cookie{Name: "homepad_session", Value: c.session})
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			resp.Body.Close()
			require.Equal(t, c.want, resp.StatusCode)
			assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
			assert.Equal(t, "frame-ancestors 'none'", resp.Header.Get("Content-Security-Policy"))
		})
	}
}
