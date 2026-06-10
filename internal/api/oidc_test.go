package api_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/oidc"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// --- mocked PocketID / OIDC provider ---------------------------------------

const (
	testClientID     = "homepad"
	testClientSecret = "test-secret"
	testRedirectURL  = "http://homepad.test/api/auth/oidc/callback"
	testKID          = "test-key-1"
)

type codeData struct {
	nonce     string
	challenge string
}

// mockIdP is a self-contained OIDC provider: it serves discovery, JWKS, the
// authorize redirect, the token endpoint, and userinfo, signing ID tokens with
// a self-generated RSA key. The identity it returns (email/groups) is set per
// test before the flow runs.
type mockIdP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	email  string
	groups []string

	codes map[string]codeData
}

func newMockIdP(t *testing.T, email string, groups []string) *mockIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	m := &mockIdP{key: key, email: email, groups: groups, codes: map[string]codeData{}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", m.handleDiscovery)
	mux.HandleFunc("GET /jwks", m.handleJWKS)
	mux.HandleFunc("GET /authorize", m.handleAuthorize)
	mux.HandleFunc("POST /token", m.handleToken)
	mux.HandleFunc("GET /userinfo", m.handleUserinfo)
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockIdP) issuer() string { return m.srv.URL }

func (m *mockIdP) config(adminGroup string) oidc.Config {
	return oidc.Config{
		Enabled:      true,
		DiscoveryURL: m.srv.URL + "/.well-known/openid-configuration",
		Issuer:       m.srv.URL,
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		RedirectURL:  testRedirectURL,
		AdminGroup:   adminGroup,
	}
}

func (m *mockIdP) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{
		"issuer":                 m.srv.URL,
		"authorization_endpoint": m.srv.URL + "/authorize",
		"token_endpoint":         m.srv.URL + "/token",
		"userinfo_endpoint":      m.srv.URL + "/userinfo",
		"jwks_uri":               m.srv.URL + "/jwks",
	})
}

func (m *mockIdP) handleJWKS(w http.ResponseWriter, r *http.Request) {
	pub := m.key.PublicKey
	writeJSON(w, map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": testKID,
			"n":   b64(pub.N.Bytes()),
			"e":   b64(big.NewInt(int64(pub.E)).Bytes()),
		}},
	})
}

// handleAuthorize mints an auth code bound to the nonce + PKCE challenge, then
// redirects back to the client's redirect_uri — exactly as PocketID would after
// the user consents.
func (m *mockIdP) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != testClientID {
		http.Error(w, "bad authorize request", http.StatusBadRequest)
		return
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "missing PKCE challenge", http.StatusBadRequest)
		return
	}
	code := "auth-code-" + q.Get("state")
	m.codes[code] = codeData{nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}

	redirect, _ := url.Parse(q.Get("redirect_uri"))
	rq := redirect.Query()
	rq.Set("code", code)
	rq.Set("state", q.Get("state"))
	redirect.RawQuery = rq.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (m *mockIdP) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.Form.Get("grant_type") != "authorization_code" ||
		r.Form.Get("client_id") != testClientID ||
		r.Form.Get("client_secret") != testClientSecret {
		http.Error(w, "bad token request", http.StatusBadRequest)
		return
	}
	cd, ok := m.codes[r.Form.Get("code")]
	if !ok {
		http.Error(w, "unknown code", http.StatusBadRequest)
		return
	}
	// Enforce PKCE: the verifier must hash to the challenge from /authorize.
	if oidc.Challenge(r.Form.Get("code_verifier")) != cd.challenge {
		http.Error(w, "PKCE verification failed", http.StatusBadRequest)
		return
	}

	idToken := m.signIDToken(cd.nonce)
	writeJSON(w, map[string]any{
		"access_token": "mock-access-token",
		"token_type":   "Bearer",
		"id_token":     idToken,
		"expires_in":   3600,
	})
}

func (m *mockIdP) handleUserinfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"sub":    "mock-subject",
		"email":  m.email,
		"groups": m.groups,
	})
}

func (m *mockIdP) signIDToken(nonce string) string {
	now := time.Now()
	claims := map[string]any{
		"iss":    m.issuer(),
		"sub":    "mock-subject",
		"aud":    testClientID,
		"exp":    now.Add(time.Hour).Unix(),
		"iat":    now.Unix(),
		"nonce":  nonce,
		"email":  m.email,
		"groups": m.groups,
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT", "kid": testKID}
	hb, _ := json.Marshal(header)
	pb, _ := json.Marshal(claims)
	signingInput := b64(hb) + "." + b64(pb)
	sum := sha256.Sum256([]byte(signingInput))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, m.key, crypto.SHA256, sum[:])
	return signingInput + "." + b64(sig)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- flow driver ------------------------------------------------------------

// runOIDCLogin drives the full browser round-trip against the mocked IdP and
// returns the homepad_session cookie the callback sets on success.
func runOIDCLogin(t *testing.T, homepad *httptest.Server, idp *mockIdP) *http.Cookie {
	t.Helper()
	// A client that surfaces each redirect instead of following it, so the test
	// can hop deliberately between homepad and the IdP.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// 1. /api/auth/oidc/login -> 302 to the IdP authorize endpoint.
	loginResp, err := client.Get(homepad.URL + "/api/auth/oidc/login")
	require.NoError(t, err)
	loginResp.Body.Close()
	require.Equal(t, http.StatusFound, loginResp.StatusCode)
	authorizeURL := loginResp.Header.Get("Location")
	require.NotEmpty(t, authorizeURL)

	au, err := url.Parse(authorizeURL)
	require.NoError(t, err)
	assert.Equal(t, "S256", au.Query().Get("code_challenge_method"))
	assert.NotEmpty(t, au.Query().Get("code_challenge"))
	assert.NotEmpty(t, au.Query().Get("state"))
	assert.NotEmpty(t, au.Query().Get("nonce"))
	assert.Contains(t, au.Query().Get("scope"), "groups")

	// 2. Hit the IdP authorize endpoint -> 302 back to homepad's redirect_uri.
	authResp, err := client.Get(authorizeURL)
	require.NoError(t, err)
	authResp.Body.Close()
	require.Equal(t, http.StatusFound, authResp.StatusCode)
	cbURL, err := url.Parse(authResp.Header.Get("Location"))
	require.NoError(t, err)
	code := cbURL.Query().Get("code")
	state := cbURL.Query().Get("state")
	require.NotEmpty(t, code)
	require.Equal(t, au.Query().Get("state"), state, "state must round-trip")

	// 3. Deliver the code to homepad's callback (the redirect_uri host is a
	//    placeholder; we issue the request to the real test server).
	cb := homepad.URL + "/api/auth/oidc/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state)
	cbResp, err := client.Get(cb)
	require.NoError(t, err)
	cbResp.Body.Close()
	require.Equal(t, http.StatusFound, cbResp.StatusCode, "callback should redirect into the app")
	assert.Equal(t, "/", cbResp.Header.Get("Location"))

	for _, c := range cbResp.Cookies() {
		if c.Name == "homepad_session" {
			return c
		}
	}
	t.Fatal("callback did not set a homepad_session cookie")
	return nil
}

// fetchMe calls /api/me with the session cookie and returns the user view.
func fetchMe(t *testing.T, homepad *httptest.Server, cookie *http.Cookie) (email, role string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, homepad.URL+"/api/me", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var v struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&v))
	return v.Email, v.Role
}

// --- tests ------------------------------------------------------------------

// New OIDC identity whose groups include OIDC_ADMIN_GROUP -> account is created
// with the admin role and a working homepad session.
func TestOIDCCallbackCreatesAdminFromGroup(t *testing.T) {
	const adminGroup = "homepad-admins"
	idp := newMockIdP(t, "newadmin@calebdunn.tech", []string{"everyone", adminGroup})
	homepad, store := testsupport.NewOIDCServer(t, idp.config(adminGroup))

	cookie := runOIDCLogin(t, homepad, idp)

	email, role := fetchMe(t, homepad, cookie)
	assert.Equal(t, "newadmin@calebdunn.tech", email)
	assert.Equal(t, "admin", role, "members of OIDC_ADMIN_GROUP must map to the homepad admin role")

	// The user really landed in the database.
	u, err := store.UserByEmail(context.Background(), "newadmin@calebdunn.tech")
	require.NoError(t, err)
	assert.Equal(t, "admin", u.Role)
}

// New OIDC identity without the admin group -> a regular user.
func TestOIDCCallbackCreatesRegularUser(t *testing.T) {
	idp := newMockIdP(t, "regular@calebdunn.tech", []string{"everyone"})
	homepad, _ := testsupport.NewOIDCServer(t, idp.config("homepad-admins"))

	cookie := runOIDCLogin(t, homepad, idp)

	email, role := fetchMe(t, homepad, cookie)
	assert.Equal(t, "regular@calebdunn.tech", email)
	assert.Equal(t, "user", role)
}

// An existing local account with the same email is linked, not duplicated, and
// its role is preserved.
func TestOIDCCallbackLinksExistingLocalUserByEmail(t *testing.T) {
	const email = "existing@calebdunn.tech"
	// Even though the OIDC groups would grant admin, linking must reuse the
	// existing local row (here a regular user) rather than create a new one.
	idp := newMockIdP(t, email, []string{"homepad-admins"})
	homepad, store := testsupport.NewOIDCServer(t, idp.config("homepad-admins"))

	ctx := context.Background()
	hash, err := bcrypt.GenerateFromPassword([]byte("local-pw"), bcrypt.MinCost)
	require.NoError(t, err)
	existing, err := store.CreateUser(ctx, email, string(hash), "user")
	require.NoError(t, err)

	cookie := runOIDCLogin(t, homepad, idp)

	gotEmail, role := fetchMe(t, homepad, cookie)
	assert.Equal(t, email, gotEmail)
	assert.Equal(t, "user", role, "linking must not change the existing user's role")

	linked, err := store.UserByEmail(ctx, email)
	require.NoError(t, err)
	assert.Equal(t, existing.ID, linked.ID, "must link to the existing row, not create a new one")
	n, err := store.CountUsers(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "no duplicate user should be created")
}

// With OIDC disabled the login/callback endpoints are not registered (404),
// and the config endpoint reports it as off.
func TestOIDCDisabledHidesEndpoints(t *testing.T) {
	homepad, _ := testsupport.NewOIDCServer(t, oidc.Config{Enabled: false})

	resp, err := http.Get(homepad.URL + "/api/auth/oidc/login")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "OIDC login must 404 when disabled")

	cfgResp, err := http.Get(homepad.URL + "/api/auth/config")
	require.NoError(t, err)
	defer cfgResp.Body.Close()
	require.Equal(t, http.StatusOK, cfgResp.StatusCode)
	var cfg struct {
		OIDCEnabled bool `json:"oidcEnabled"`
	}
	require.NoError(t, json.NewDecoder(cfgResp.Body).Decode(&cfg))
	assert.False(t, cfg.OIDCEnabled)
}

// When OIDC is enabled the config endpoint advertises it so the web client can
// show the PocketID button.
func TestOIDCEnabledConfigAdvertised(t *testing.T) {
	idp := newMockIdP(t, "x@calebdunn.tech", nil)
	homepad, _ := testsupport.NewOIDCServer(t, idp.config(""))

	resp, err := http.Get(homepad.URL + "/api/auth/config")
	require.NoError(t, err)
	defer resp.Body.Close()
	var cfg struct {
		OIDCEnabled bool `json:"oidcEnabled"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&cfg))
	assert.True(t, cfg.OIDCEnabled)
}
