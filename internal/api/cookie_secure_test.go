package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/dbguard"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// TestSessionCookieSecure locks issue #14: the session cookie's Secure attribute
// is gated by COOKIE_SECURE and defaults to ON. Unset or "true" -> Secure set;
// "false" -> Secure absent (for the plain-HTTP nip.io staging setup). HttpOnly
// and SameSite=Lax must stay set either way. Login, the sliding-expiry refresh
// and logout are all checked.
func TestSessionCookieSecure(t *testing.T) {
	dsn := dbguard.RequireTestDatabase(t)

	ctx := context.Background()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	const email, password = "secure@homepad.test", "supersecret"

	cases := []struct {
		name       string
		setEnv     bool
		env        string
		wantSecure bool
	}{
		{"unset defaults to secure", false, "", true},
		{"COOKIE_SECURE=true", true, "true", true},
		{"COOKIE_SECURE=false", true, "false", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setEnv {
				t.Setenv("COOKIE_SECURE", tc.env)
			} else {
				os.Unsetenv("COOKIE_SECURE")
			}

			truncateRegistration(t, ctx, dsn)
			hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
			if err != nil {
				t.Fatalf("hash: %v", err)
			}
			if _, err := store.CreateUser(ctx, email, string(hash), "admin"); err != nil {
				t.Fatalf("seed user: %v", err)
			}

			clk := newTestClock()
			sessions := session.NewMemoryStore()
			sessions.Clock = clk.Now
			h := api.New(api.Deps{Store: store, Sessions: sessions})
			srv := httptest.NewServer(h)
			defer srv.Close()

			// Login sets the session cookie.
			body, _ := json.Marshal(map[string]string{"email": email, "password": password})
			loginResp, err := http.Post(srv.URL+"/api/login", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST /api/login: %v", err)
			}
			loginResp.Body.Close()
			assertSessionCookie(t, "login", loginResp.Cookies(), tc.wantSecure)

			// A slide re-issues the cookie (AC-010); it must carry the same
			// flags as the login cookie.
			var token string
			for _, c := range loginResp.Cookies() {
				if c.Name == "homepad_session" {
					token = c.Value
				}
			}
			clk.Advance(session.TouchInterval)
			meReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/me", nil)
			meReq.AddCookie(&http.Cookie{Name: "homepad_session", Value: token})
			meResp, err := http.DefaultClient.Do(meReq)
			if err != nil {
				t.Fatalf("GET /api/me: %v", err)
			}
			meResp.Body.Close()
			if meResp.StatusCode != http.StatusOK {
				t.Fatalf("GET /api/me after a minute = %d, want 200", meResp.StatusCode)
			}
			assertSessionCookie(t, "refresh", meResp.Cookies(), tc.wantSecure)

			// Logout sets the cleared cookie and must mirror the same flags.
			logoutResp, err := http.Post(srv.URL+"/api/logout", "application/json", nil)
			if err != nil {
				t.Fatalf("POST /api/logout: %v", err)
			}
			logoutResp.Body.Close()
			assertSessionCookie(t, "logout", logoutResp.Cookies(), tc.wantSecure)
		})
	}
}

func assertSessionCookie(t *testing.T, which string, cookies []*http.Cookie, wantSecure bool) {
	t.Helper()
	var ck *http.Cookie
	for _, c := range cookies {
		if c.Name == "homepad_session" {
			ck = c
			break
		}
	}
	if ck == nil {
		t.Fatalf("%s: no homepad_session cookie set", which)
	}
	if ck.Secure != wantSecure {
		t.Errorf("%s: cookie Secure = %v, want %v", which, ck.Secure, wantSecure)
	}
	if !ck.HttpOnly {
		t.Errorf("%s: cookie HttpOnly = false, want true", which)
	}
	if ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("%s: cookie SameSite = %v, want Lax", which, ck.SameSite)
	}
}
