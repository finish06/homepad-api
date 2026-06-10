// Package oidc implements the homelab PocketID (OpenID Connect) login flow:
// Authorization Code with PKCE, ID-token verification via the provider's JWKS,
// and an in-memory store for the short-lived per-login state (verifier+nonce).
//
// It is deliberately self-contained — ID-token signature verification is done
// against the discovery document's jwks_uri with stdlib crypto (see verify.go),
// so no external OIDC/JWT dependency is pulled in.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Config holds everything needed to talk to PocketID. Every field is sourced
// from the environment (see ConfigFromEnv) so real values are wired at deploy.
type Config struct {
	Enabled      bool
	DiscoveryURL string   // OIDC_DISCOVERY_URL; if empty, derived from Issuer.
	Issuer       string   // OIDC_ISSUER; also the expected `iss` of the ID token.
	ClientID     string   // OIDC_CLIENT_ID
	ClientSecret string   // OIDC_CLIENT_SECRET
	RedirectURL  string   // OIDC_REDIRECT_URL (the /api/auth/oidc/callback URL)
	AdminGroup   string   // OIDC_ADMIN_GROUP; members get the homepad admin role.
	Scopes       []string // defaults to openid profile email groups
}

// ConfigFromEnv reads the OIDC_* environment variables. OIDC is off unless
// OIDC_ENABLED is a truthy value (1/true/yes/on).
func ConfigFromEnv() Config {
	c := Config{
		Enabled:      truthy(os.Getenv("OIDC_ENABLED")),
		DiscoveryURL: os.Getenv("OIDC_DISCOVERY_URL"),
		Issuer:       strings.TrimRight(os.Getenv("OIDC_ISSUER"), "/"),
		ClientID:     os.Getenv("OIDC_CLIENT_ID"),
		ClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
		RedirectURL:  os.Getenv("OIDC_REDIRECT_URL"),
		AdminGroup:   os.Getenv("OIDC_ADMIN_GROUP"),
		Scopes:       []string{"openid", "profile", "email", "groups"},
	}
	return c
}

// discoveryURL is the well-known document URL: the explicit override if set,
// otherwise issuer + the standard path.
func (c Config) discoveryURL() string {
	if c.DiscoveryURL != "" {
		return c.DiscoveryURL
	}
	return c.Issuer + "/.well-known/openid-configuration"
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// metadata is the subset of the OIDC discovery document homepad uses.
type metadata struct {
	Issuer        string `json:"issuer"`
	AuthEndpoint  string `json:"authorization_endpoint"`
	TokenEndpoint string `json:"token_endpoint"`
	UserinfoURL   string `json:"userinfo_endpoint"`
	JWKSURI       string `json:"jwks_uri"`
}

// Provider talks to one OIDC issuer. Discovery and JWKS are fetched lazily on
// first use and cached, so constructing a Provider does no network I/O and a
// briefly-unreachable IdP doesn't block boot.
type Provider struct {
	cfg Config
	hc  *http.Client

	mu   sync.Mutex
	meta *metadata
	keys map[string]*rsaKey // by kid
}

// NewProvider returns a Provider for cfg. It does not perform any I/O.
func NewProvider(cfg Config) *Provider {
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"openid", "profile", "email", "groups"}
	}
	return &Provider{
		cfg: cfg,
		hc:  &http.Client{Timeout: 10 * time.Second},
	}
}

// discover loads and caches the discovery document.
func (p *Provider) discover(ctx context.Context) (*metadata, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.meta != nil {
		return p.meta, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.discoveryURL(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc: discovery: status %d", resp.StatusCode)
	}
	var m metadata
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("oidc: discovery decode: %w", err)
	}
	if m.AuthEndpoint == "" || m.TokenEndpoint == "" || m.JWKSURI == "" {
		return nil, fmt.Errorf("oidc: discovery missing required endpoints")
	}
	p.meta = &m
	return &m, nil
}

// AuthCodeURL builds the authorize redirect for the login leg, carrying the
// PKCE challenge, state, and nonce.
func (p *Provider) AuthCodeURL(ctx context.Context, state, nonce, challenge string) (string, error) {
	m, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(m.AuthEndpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", p.cfg.ClientID)
	q.Set("redirect_uri", p.cfg.RedirectURL)
	q.Set("scope", strings.Join(p.cfg.Scopes, " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// tokenResponse is the subset of the token endpoint response we consume.
type tokenResponse struct {
	IDToken     string `json:"id_token"`
	AccessToken string `json:"access_token"`
}

// Exchange swaps an authorization code for tokens, supplying the PKCE verifier.
func (p *Provider) Exchange(ctx context.Context, code, verifier string) (idToken, accessToken string, err error) {
	m, err := p.discover(ctx)
	if err != nil {
		return "", "", err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {p.cfg.RedirectURL},
		"client_id":     {p.cfg.ClientID},
		"client_secret": {p.cfg.ClientSecret},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("oidc: token exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", fmt.Errorf("oidc: token exchange: status %d: %s", resp.StatusCode, body)
	}
	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", "", fmt.Errorf("oidc: token decode: %w", err)
	}
	if tr.IDToken == "" {
		return "", "", fmt.Errorf("oidc: token response had no id_token")
	}
	return tr.IDToken, tr.AccessToken, nil
}

// Claims holds the identity facts homepad needs from a login.
type Claims struct {
	Email  string   `json:"email"`
	Groups []string `json:"groups"`
}

// Userinfo fetches the userinfo endpoint with accessToken. It is used as a
// fallback when the ID token omits email or groups. Returns empty Claims (no
// error) when the provider exposes no userinfo endpoint.
func (p *Provider) Userinfo(ctx context.Context, accessToken string) (Claims, error) {
	m, err := p.discover(ctx)
	if err != nil {
		return Claims{}, err
	}
	if m.UserinfoURL == "" {
		return Claims{}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.UserinfoURL, nil)
	if err != nil {
		return Claims{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return Claims{}, fmt.Errorf("oidc: userinfo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Claims{}, fmt.Errorf("oidc: userinfo: status %d", resp.StatusCode)
	}
	var c Claims
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return Claims{}, fmt.Errorf("oidc: userinfo decode: %w", err)
	}
	return c, nil
}

// --- PKCE / state / nonce helpers ---

// RandToken returns a URL-safe random token suitable for state and nonce.
func RandToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// NewVerifier returns a PKCE code_verifier (43-char base64url of 32 bytes).
func NewVerifier() string { return RandToken() }

// Challenge derives the S256 PKCE code_challenge from a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// --- pending-login store ---

type pendingEntry struct {
	verifier string
	nonce    string
	expires  time.Time
}

// Pending is an in-memory store of in-flight logins keyed by state. It mirrors
// the in-memory session.Manager design: v1 runs a single replica, so a restart
// simply invalidates logins that were mid-redirect, which is acceptable.
type Pending struct {
	mu  sync.Mutex
	m   map[string]pendingEntry
	ttl time.Duration
}

// NewPending returns a store whose entries expire after ttl.
func NewPending(ttl time.Duration) *Pending {
	return &Pending{m: map[string]pendingEntry{}, ttl: ttl}
}

// Put records the verifier and nonce for state.
func (p *Pending) Put(state, verifier, nonce string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[state] = pendingEntry{verifier: verifier, nonce: nonce, expires: time.Now().Add(p.ttl)}
}

// Take consumes the entry for state, returning false if it is missing or
// expired. An entry is single-use.
func (p *Pending) Take(state string) (verifier, nonce string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, found := p.m[state]
	if !found {
		return "", "", false
	}
	delete(p.m, state)
	if time.Now().After(e.expires) {
		return "", "", false
	}
	return e.verifier, e.nonce, true
}
