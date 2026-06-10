package oidc

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// rsaKey is one parsed JWKS signing key.
type rsaKey struct {
	kid string
	pub *rsa.PublicKey
}

// jwks is the JSON Web Key Set document.
type jwks struct {
	Keys []struct {
		Kid string `json:"kid"`
		Kty string `json:"kty"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

// idTokenClaims is the set of registered + PocketID claims homepad validates.
type idTokenClaims struct {
	Iss    string          `json:"iss"`
	Aud    json.RawMessage `json:"aud"` // string or []string
	Exp    int64           `json:"exp"`
	Nonce  string          `json:"nonce"`
	Email  string          `json:"email"`
	Groups []string        `json:"groups"`
}

// VerifyIDToken validates a raw JWT ID token: RS256 signature against the
// provider JWKS, then issuer, audience, expiry, and nonce. On success it
// returns the email and groups claims.
func (p *Provider) VerifyIDToken(ctx context.Context, raw, expectedNonce string) (Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Claims{}, fmt.Errorf("oidc: malformed id_token")
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, fmt.Errorf("oidc: id_token header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return Claims{}, fmt.Errorf("oidc: id_token header decode: %w", err)
	}
	if header.Alg != "RS256" {
		return Claims{}, fmt.Errorf("oidc: unsupported id_token alg %q", header.Alg)
	}

	key, err := p.signingKey(ctx, header.Kid)
	if err != nil {
		return Claims{}, err
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("oidc: id_token signature: %w", err)
	}
	signingInput := []byte(parts[0] + "." + parts[1])
	digest := sha256.Sum256(signingInput)
	if err := rsa.VerifyPKCS1v15(key.pub, crypto.SHA256, digest[:], sig); err != nil {
		return Claims{}, fmt.Errorf("oidc: id_token signature invalid: %w", err)
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, fmt.Errorf("oidc: id_token payload: %w", err)
	}
	var c idTokenClaims
	if err := json.Unmarshal(payloadJSON, &c); err != nil {
		return Claims{}, fmt.Errorf("oidc: id_token payload decode: %w", err)
	}

	if p.cfg.Issuer != "" && c.Iss != p.cfg.Issuer {
		return Claims{}, fmt.Errorf("oidc: id_token iss %q != expected %q", c.Iss, p.cfg.Issuer)
	}
	if !audienceContains(c.Aud, p.cfg.ClientID) {
		return Claims{}, fmt.Errorf("oidc: id_token aud does not include client_id")
	}
	if c.Exp != 0 && time.Now().After(time.Unix(c.Exp, 0)) {
		return Claims{}, fmt.Errorf("oidc: id_token expired")
	}
	if expectedNonce != "" && c.Nonce != expectedNonce {
		return Claims{}, fmt.Errorf("oidc: id_token nonce mismatch")
	}

	return Claims{Email: c.Email, Groups: c.Groups}, nil
}

// signingKey returns the JWKS key for kid, refreshing the cached key set once
// if the kid is unknown (handles provider key rotation).
func (p *Provider) signingKey(ctx context.Context, kid string) (*rsaKey, error) {
	p.mu.Lock()
	k, ok := p.keys[kid]
	p.mu.Unlock()
	if ok {
		return k, nil
	}

	if err := p.refreshKeys(ctx); err != nil {
		return nil, err
	}

	p.mu.Lock()
	k, ok = p.keys[kid]
	p.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("oidc: no JWKS key for kid %q", kid)
	}
	return k, nil
}

func (p *Provider) refreshKeys(ctx context.Context) error {
	m, err := p.discover(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.JWKSURI, nil)
	if err != nil {
		return err
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return fmt.Errorf("oidc: jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oidc: jwks: status %d", resp.StatusCode)
	}
	var set jwks
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return fmt.Errorf("oidc: jwks decode: %w", err)
	}

	keys := make(map[string]*rsaKey, len(set.Keys))
	for _, jk := range set.Keys {
		if jk.Kty != "RSA" {
			continue
		}
		pub, err := rsaPublicKey(jk.N, jk.E)
		if err != nil {
			return fmt.Errorf("oidc: jwks key %q: %w", jk.Kid, err)
		}
		keys[jk.Kid] = &rsaKey{kid: jk.Kid, pub: pub}
	}

	p.mu.Lock()
	p.keys = keys
	p.mu.Unlock()
	return nil
}

// rsaPublicKey builds an *rsa.PublicKey from base64url-encoded modulus and
// exponent (the JWKS `n` and `e` fields).
func rsaPublicKey(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, fmt.Errorf("modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, fmt.Errorf("exponent: %w", err)
	}
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() || e.Int64() <= 0 {
		return nil, fmt.Errorf("invalid exponent")
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(e.Int64()),
	}, nil
}

// audienceContains reports whether the `aud` claim (a string or array of
// strings) includes want.
func audienceContains(raw json.RawMessage, want string) bool {
	if len(raw) == 0 {
		return false
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == want {
				return true
			}
		}
	}
	return false
}
