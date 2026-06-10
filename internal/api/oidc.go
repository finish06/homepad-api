package api

import (
	"context"
	"errors"
	"log"
	"net/http"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/oidc"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// oidcNoPassword is stored in users.password_hash for accounts provisioned via
// PocketID. It is not a valid bcrypt hash, so bcrypt.CompareHashAndPassword
// always fails for these rows — an OIDC-only user can never local-login, while
// a user who also set a local password keeps it (we link by email, never
// overwrite the hash).
const oidcNoPassword = "oidc:no-local-password"

// handleOIDCConfig reports whether OIDC login is available, so the web client
// can show or hide the "Log in with PocketID" button. It is always registered
// (even when OIDC is disabled) and never requires a session.
func (s *server) handleOIDCConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"oidcEnabled": s.oidc.Enabled})
}

// handleOIDCLogin begins the Authorization Code + PKCE flow: it mints state,
// nonce, and a PKCE verifier, stashes them server-side keyed by state, and
// redirects the browser to the PocketID authorize endpoint.
func (s *server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	state := oidc.RandToken()
	nonce := oidc.RandToken()
	verifier := oidc.NewVerifier()

	authURL, err := s.provider.AuthCodeURL(r.Context(), state, nonce, oidc.Challenge(verifier))
	if err != nil {
		log.Printf("oidc login: build authorize url: %v", err)
		http.Error(w, "oidc provider unavailable", http.StatusBadGateway)
		return
	}

	s.pending.Put(state, verifier, nonce)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleOIDCCallback completes the flow: validate state, exchange the code with
// the PKCE verifier, verify the ID token, resolve the user by email (linking to
// an existing local account or creating a new one with the right role), then set
// the same homepad_session cookie the local login uses and land on the catalog.
func (s *server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	if state == "" || code == "" {
		http.Error(w, "missing state or code", http.StatusBadRequest)
		return
	}

	verifier, nonce, ok := s.pending.Take(state)
	if !ok {
		http.Error(w, "invalid or expired login state", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	idToken, accessToken, err := s.provider.Exchange(ctx, code, verifier)
	if err != nil {
		log.Printf("oidc callback: token exchange: %v", err)
		http.Error(w, "oidc token exchange failed", http.StatusBadGateway)
		return
	}

	claims, err := s.provider.VerifyIDToken(ctx, idToken, nonce)
	if err != nil {
		log.Printf("oidc callback: verify id_token: %v", err)
		http.Error(w, "invalid id token", http.StatusUnauthorized)
		return
	}

	// The ID token is the primary identity source; fall back to userinfo only
	// when it omits the email or groups we need.
	if claims.Email == "" || len(claims.Groups) == 0 {
		if ui, err := s.provider.Userinfo(ctx, accessToken); err == nil {
			if claims.Email == "" {
				claims.Email = ui.Email
			}
			if len(claims.Groups) == 0 {
				claims.Groups = ui.Groups
			}
		}
	}
	if claims.Email == "" {
		http.Error(w, "oidc identity has no email", http.StatusUnauthorized)
		return
	}

	u, err := s.resolveOIDCUser(ctx, claims)
	if err != nil {
		log.Printf("oidc callback: resolve user: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	token, err := s.sessions.Create(u.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// resolveOIDCUser links the OIDC identity to a homepad user by email: an
// existing local account is reused as-is; otherwise a new account is created,
// with the admin role when the user's groups include OIDC_ADMIN_GROUP.
func (s *server) resolveOIDCUser(ctx context.Context, claims oidc.Claims) (storage.User, error) {
	u, err := s.store.UserByEmail(ctx, claims.Email)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return storage.User{}, err
	}

	role := "user"
	if s.oidc.AdminGroup != "" && containsString(claims.Groups, s.oidc.AdminGroup) {
		role = "admin"
	}
	return s.store.CreateUser(ctx, claims.Email, oidcNoPassword, role)
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
