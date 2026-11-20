package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// Passwords at the bcrypt boundary. bcrypt reads at most 72 bytes; the limit
// is bytes, so multibyte characters reach it in fewer characters.
var (
	pw72Bytes = strings.Repeat("é", 36)       // 36 characters, 72 bytes
	pw73Bytes = strings.Repeat("€", 24) + "a" // 25 characters, 73 bytes
)

func TestPasswordFixturesAreAtTheBoundary(t *testing.T) {
	require.Equal(t, 72, len(pw72Bytes))
	require.Equal(t, 73, len(pw73Bytes))
}

// Register takes a 72-byte password and refuses a 73-byte one with a 400
// that says why, rather than a 500 from bcrypt.
func TestRegister_PasswordByteLimit(t *testing.T) {
	env := testsupport.NewEnv(t)

	r := postJSON(t, env.URL+"/api/register", map[string]string{"email": "long@example.com", "password": pw73Bytes})
	b := readAll(t, r)
	require.Equal(t, http.StatusBadRequest, r.StatusCode, string(b))
	assert.Contains(t, string(b), "72 bytes")

	r = postJSON(t, env.URL+"/api/register", map[string]string{"email": "edge@example.com", "password": pw72Bytes})
	b = readAll(t, r)
	require.Equal(t, http.StatusCreated, r.StatusCode, string(b))

	r = postJSON(t, env.URL+"/api/login", map[string]string{"email": "edge@example.com", "password": pw72Bytes})
	b = readAll(t, r)
	assert.Equal(t, http.StatusOK, r.StatusCode, "a 72-byte password signs in: %s", b)
}

// Invite accept has the same limit, and a refused password leaves the
// invite usable.
func TestInviteAccept_PasswordByteLimit(t *testing.T) {
	env := inviteEnv(t)
	inv := mustInvite(t, env, "Robin", "user")

	body := acceptBody(inv.Token, "robin@example.com")
	body["password"] = pw73Bytes
	r, b := acceptInvite(t, env, body)
	require.Equal(t, http.StatusBadRequest, r.StatusCode, string(b))
	assert.Contains(t, string(b), "72 bytes")

	body["password"] = pw72Bytes
	r, b = acceptInvite(t, env, body)
	require.Equal(t, http.StatusCreated, r.StatusCode, "the invite is still usable: %s", b)

	login := postJSON(t, env.URL+"/api/login", map[string]string{"email": "robin@example.com", "password": pw72Bytes})
	readAll(t, login)
	assert.Equal(t, http.StatusOK, login.StatusCode)
}

// Login with a password over 72 bytes is an ordinary 401. That includes one
// whose first 72 bytes are the real password: bcrypt alone would ignore the
// extra bytes and let it in.
func TestLogin_OverLongPasswordIs401(t *testing.T) {
	env := testsupport.NewEnv(t)
	r := postJSON(t, env.URL+"/api/register", map[string]string{"email": "edge@example.com", "password": pw72Bytes})
	b := readAll(t, r)
	require.Equal(t, http.StatusCreated, r.StatusCode, string(b))

	for _, tc := range []struct{ name, email, password string }{
		{"real password plus one byte", "edge@example.com", pw72Bytes + "x"},
		{"long wrong password", "edge@example.com", strings.Repeat("z", 200)},
		{"long password, unknown email", "nobody@example.com", strings.Repeat("z", 200)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := postJSON(t, env.URL+"/api/login", map[string]string{"email": tc.email, "password": tc.password})
			b := readAll(t, r)
			assert.Equal(t, http.StatusUnauthorized, r.StatusCode, string(b))
			assert.Equal(t, "invalid email or password\n", string(b), "the same generic 401 as any failed login")
			for _, c := range r.Cookies() {
				assert.NotEqual(t, "homepad_session", c.Name, "a failed login sets no session")
			}
		})
	}
}

// An unknown email, an OIDC-only account and a wrong password all get the
// same generic 401.
func TestLogin_FailuresLookAlike(t *testing.T) {
	const adminGroup = "homepad-admins"
	idp := newMockIdP(t, "sso-only@example.com", []string{"everyone"})
	homepad, _ := testsupport.NewOIDCServer(t, idp.config(adminGroup))
	runOIDCLogin(t, homepad, idp) // creates the OIDC-only account

	r := postJSON(t, homepad.URL+"/api/register", map[string]string{"email": "local@example.com", "password": "a long enough password"})
	b := readAll(t, r)
	require.Equal(t, http.StatusCreated, r.StatusCode, string(b))

	for _, tc := range []struct{ name, email string }{
		{"unknown email", "nobody@example.com"},
		{"OIDC-only account", "sso-only@example.com"},
		{"wrong password", "local@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := postJSON(t, homepad.URL+"/api/login", map[string]string{"email": tc.email, "password": "not the password"})
			b := readAll(t, r)
			assert.Equal(t, http.StatusUnauthorized, r.StatusCode)
			assert.Equal(t, "invalid email or password\n", string(b))
		})
	}
}

// The OIDC callback's session cookie follows COOKIE_SECURE exactly like the
// password login's.
func TestOIDCCallbackCookie_FollowsCookieSecure(t *testing.T) {
	for _, tc := range []struct {
		env        string
		wantSecure bool
	}{
		{"true", true},
		{"false", false},
	} {
		t.Run("COOKIE_SECURE="+tc.env, func(t *testing.T) {
			t.Setenv("COOKIE_SECURE", tc.env)
			idp := newMockIdP(t, "sso-"+tc.env+"@example.com", []string{"everyone"})
			homepad, _ := testsupport.NewOIDCServer(t, idp.config("homepad-admins"))

			cookie := runOIDCLogin(t, homepad, idp)
			assert.Equal(t, tc.wantSecure, cookie.Secure, "OIDC callback cookie Secure")
			assert.True(t, cookie.HttpOnly)
			assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)

			// Password login on the same server sets the same flags.
			r := postJSON(t, homepad.URL+"/api/register", map[string]string{"email": "pw-" + tc.env + "@example.com", "password": "a long enough password"})
			readAll(t, r)
			require.Equal(t, http.StatusCreated, r.StatusCode)
			login := postJSON(t, homepad.URL+"/api/login", map[string]string{"email": "pw-" + tc.env + "@example.com", "password": "a long enough password"})
			readAll(t, login)
			require.Equal(t, http.StatusOK, login.StatusCode)
			var pwCookie *http.Cookie
			for _, c := range login.Cookies() {
				if c.Name == "homepad_session" {
					pwCookie = c
				}
			}
			require.NotNil(t, pwCookie)
			assert.Equal(t, pwCookie.Secure, cookie.Secure, "OIDC and password sign-in set the same Secure flag")
		})
	}
}
