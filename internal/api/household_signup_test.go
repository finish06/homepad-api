package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-household-basics scope 1 — safe sign-up by default (AC-001…AC-004,
// AC-018's first-admin line).

// lockedBuffer is a log sink safe to read while handler goroutines write.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog sends the standard logger to a buffer for the rest of the test.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

func modePtr(m api.RegistrationMode) *api.RegistrationMode { return &m }

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	return doNoCookie(t, http.MethodPost, url, body)
}

type authConfigView struct {
	OIDCEnabled  bool   `json:"oidcEnabled"`
	Registration string `json:"registration"`
	SetupNeeded  bool   `json:"setupNeeded"`
}

func getAuthConfig(t *testing.T, baseURL string) (authConfigView, map[string]any) {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/auth/config")
	require.NoError(t, err)
	b := readAll(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	var v authConfigView
	require.NoError(t, json.Unmarshal(b, &v))
	var raw map[string]any
	require.NoError(t, json.Unmarshal(b, &raw))
	return v, raw
}

// AC-001 — the API default is invite_only when HOMEPAD_REGISTRATION is unset
// or empty. Explicit values keep their meaning, and junk still fails closed
// (recognized=false so main can warn).
func TestRegistrationFromEnv_DefaultIsInviteOnly(t *testing.T) {
	assert.Equal(t, "invite_only", api.DefaultRegistration)
	for _, tc := range []struct {
		raw        string
		want       api.RegistrationMode
		recognized bool
	}{
		{"", api.RegistrationClosed, true},
		{"   ", api.RegistrationClosed, true},
		{"invite_only", api.RegistrationClosed, true},
		{"invite-only", api.RegistrationClosed, true},
		{"open", api.RegistrationOpen, true},
		{" Open ", api.RegistrationOpen, true},
		{"nonsense", api.RegistrationClosed, false},
	} {
		got, ok := api.RegistrationFromEnv(tc.raw)
		assert.Equal(t, tc.want, got, "HOMEPAD_REGISTRATION=%q", tc.raw)
		assert.Equal(t, tc.recognized, ok, "HOMEPAD_REGISTRATION=%q recognized", tc.raw)
	}
}

// AC-004 — /api/auth/config adds registration and setupNeeded, stays public,
// and oidcEnabled is unchanged.
func TestAuthConfig_RegistrationAndSetupNeeded(t *testing.T) {
	t.Run("fresh install, invite_only", func(t *testing.T) {
		env := testsupport.NewEnvWith(t, testsupport.Options{Empty: true, Registration: modePtr(api.RegistrationClosed)})
		cfg, raw := getAuthConfig(t, env.URL)
		assert.Equal(t, "invite_only", cfg.Registration)
		assert.True(t, cfg.SetupNeeded, "no adult account yet")
		assert.False(t, cfg.OIDCEnabled)
		assert.Contains(t, raw, "oidcEnabled", "oidcEnabled is still present")
	})
	t.Run("set up, open", func(t *testing.T) {
		env := testsupport.NewEnv(t) // open, seeded adults
		cfg, _ := getAuthConfig(t, env.URL)
		assert.Equal(t, "open", cfg.Registration)
		assert.False(t, cfg.SetupNeeded)
	})
	t.Run("only kids exist: setup is still needed", func(t *testing.T) {
		env := testsupport.NewEnvWith(t, testsupport.Options{Empty: true, Registration: modePtr(api.RegistrationClosed)})
		_, err := env.Store.CreateKid(t.Context(), "Sam", "blue")
		require.NoError(t, err)
		cfg, _ := getAuthConfig(t, env.URL)
		assert.True(t, cfg.SetupNeeded, "setupNeeded counts adults only")
	})
}

// AC-002 — with no adult accounts, register creates an admin in every mode.
// With one or more adults, invite_only is 403 and open creates a user.
func TestRegister_FirstAccountIsAdminInEveryMode(t *testing.T) {
	for _, mode := range []api.RegistrationMode{api.RegistrationClosed, api.RegistrationOpen} {
		t.Run(fmt.Sprintf("mode %d", mode), func(t *testing.T) {
			env := testsupport.NewEnvWith(t, testsupport.Options{Empty: true, Registration: modePtr(mode)})
			resp := postJSON(t, env.URL+"/api/register", map[string]string{
				"email": "first@homepad.test", "password": "correct horse battery",
			})
			b := readAll(t, resp)
			require.Equal(t, http.StatusCreated, resp.StatusCode, string(b))
			var u struct {
				Role string `json:"role"`
			}
			require.NoError(t, json.Unmarshal(b, &u))
			assert.Equal(t, "admin", u.Role)

			second := postJSON(t, env.URL+"/api/register", map[string]string{
				"email": "second@homepad.test", "password": "correct horse battery",
			})
			sb := readAll(t, second)
			if mode == api.RegistrationOpen {
				require.Equal(t, http.StatusCreated, second.StatusCode, string(sb))
				assert.Contains(t, string(sb), `"role":"user"`)
			} else {
				assert.Equal(t, http.StatusForbidden, second.StatusCode, string(sb))
			}
		})
	}
}

// AC-003 — the bootstrap is atomic: 10 concurrent registrations on an empty
// install make exactly one admin; under invite_only the rest get 403.
func TestRegister_ConcurrentBootstrapMakesOneAdmin(t *testing.T) {
	for _, mode := range []api.RegistrationMode{api.RegistrationClosed, api.RegistrationOpen} {
		t.Run(fmt.Sprintf("mode %d", mode), func(t *testing.T) {
			env := testsupport.NewEnvWith(t, testsupport.Options{Empty: true, Registration: modePtr(mode)})
			const n = 10
			codes := make([]int, n)
			roles := make([]string, n)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					body, _ := json.Marshal(map[string]string{
						"email":    fmt.Sprintf("racer%d@homepad.test", i),
						"password": "correct horse battery",
					})
					resp, err := http.Post(env.URL+"/api/register", "application/json", bytes.NewReader(body))
					if err != nil {
						return
					}
					defer resp.Body.Close()
					codes[i] = resp.StatusCode
					var u struct {
						Role string `json:"role"`
					}
					_ = json.NewDecoder(resp.Body).Decode(&u)
					roles[i] = u.Role
				}(i)
			}
			close(start)
			wg.Wait()

			admins, forbidden, users := 0, 0, 0
			for i := 0; i < n; i++ {
				switch {
				case codes[i] == http.StatusCreated && roles[i] == "admin":
					admins++
				case codes[i] == http.StatusCreated && roles[i] == "user":
					users++
				case codes[i] == http.StatusForbidden:
					forbidden++
				}
			}
			assert.Equal(t, 1, admins, "exactly one registration becomes admin (codes %v roles %v)", codes, roles)
			if mode == api.RegistrationClosed {
				assert.Equal(t, n-1, forbidden, "under invite_only the rest are 403")
			} else {
				assert.Equal(t, n-1, users, "under open the rest are members")
			}

			var dbAdmins int
			require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
				`SELECT count(*) FROM users WHERE role = 'admin'`).Scan(&dbAdmins))
			assert.Equal(t, 1, dbAdmins, "one admin row in the database")
		})
	}
}

// AC-018 — the first-admin bootstrap writes one log line, with no password.
func TestRegister_FirstAccountLogs(t *testing.T) {
	logs := captureLog(t)
	env := testsupport.NewEnvWith(t, testsupport.Options{Empty: true, Registration: modePtr(api.RegistrationClosed)})
	resp := postJSON(t, env.URL+"/api/register", map[string]string{
		"email": "first@homepad.test", "password": "correct horse battery",
	})
	b := readAll(t, resp)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(b))
	var u struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(b, &u))

	out := logs.String()
	assert.Equal(t, 1, strings.Count(out, "register: first account "+u.ID+" created as admin"), out)
	assert.NotContains(t, out, "correct horse battery")
}
