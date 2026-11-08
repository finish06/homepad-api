package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/oidc"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// oidcNoPassword is stored in users.password_hash for accounts provisioned via
// PocketID. It is not a valid bcrypt hash, so bcrypt.CompareHashAndPassword
// always fails for these rows — an OIDC-only user can never local-login, while
// a user who also set a local password keeps it (we link by email, never
// overwrite the hash).
const oidcNoPassword = "oidc:no-local-password"

// handleOIDCConfig is GET /api/auth/config: what the sign-in screen needs
// before anyone is signed in. oidcEnabled shows or hides the "Log in with
// PocketID" button; registration ("open" or "invite_only") decides whether
// "Create account" shows; setupNeeded (no adult account yet) turns the screen
// into "Set up Homepad" (SPEC-household-basics AC-004). Always registered,
// never requires a session.
func (s *server) handleOIDCConfig(w http.ResponseWriter, r *http.Request) {
	registration := "invite_only"
	if s.registration == RegistrationOpen {
		registration = "open"
	}
	adults, err := s.store.CountUsers(r.Context())
	if err != nil {
		// Fail toward the normal sign-in screen, never toward setup.
		log.Printf("auth config: count adults: %v", err)
		adults = 1
	}
	writeJSON(w, http.StatusOK, struct {
		OIDCEnabled  bool   `json:"oidcEnabled"`
		Registration string `json:"registration"`
		SetupNeeded  bool   `json:"setupNeeded"`
	}{s.oidc.Enabled, registration, adults == 0})
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
	// when it omits the email, its email_verified claim, or the groups.
	if claims.Email == "" || claims.EmailVerified == nil || len(claims.Groups) == 0 {
		if ui, err := s.provider.Userinfo(ctx, accessToken); err == nil {
			switch {
			case claims.Email == "":
				// The email comes from userinfo, so its verification must too.
				claims.Email, claims.EmailVerified = ui.Email, ui.EmailVerified
			case claims.EmailVerified == nil && strings.EqualFold(ui.Email, claims.Email):
				// A userinfo claim counts only for the address it describes.
				claims.EmailVerified = ui.EmailVerified
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
	// #89 — an account is linked or created by email only when the provider
	// says the email is verified. Otherwise anyone who could set an address
	// at the provider could sign in as the local account that has it.
	if claims.EmailVerified == nil || !*claims.EmailVerified {
		log.Printf("oidc callback: refused sign-in: email_verified is not true")
		http.Error(w, "oidc email is not verified", http.StatusForbidden)
		return
	}

	u, err := s.resolveOIDCUser(ctx, claims)
	if errors.Is(err, storage.ErrAccountLimit) {
		s.refuseAtLimit(w, r)
		return
	}
	if err != nil {
		log.Printf("oidc callback: resolve user: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Secure stays off here, exactly as before persistent sessions: this
	// callback has never honoured COOKIE_SECURE. Aligning it with local login
	// is a separate, deliberate change.
	if !s.startSession(w, r, u, false) {
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// resolveOIDCUser links the OIDC identity to a homepad user by email: an
// existing local account is reused as-is; otherwise a new account is created,
// with the admin role when the user's groups include OIDC_ADMIN_GROUP, within
// HOMEPAD_MAX_ACCOUNTS (storage.ErrAccountLimit). The caller has checked that
// the email is verified.
func (s *server) resolveOIDCUser(ctx context.Context, claims oidc.Claims) (storage.User, error) {
	u, err := s.store.UserByEmail(ctx, claims.Email)
	if err == nil {
		// UserByEmail never returns a kid; refuse one anyway rather than sign
		// a kid in through an identity provider.
		if !storage.IsAdultRole(u.Role) {
			return storage.User{}, errors.New("oidc: identity resolves to a non-adult account")
		}
		return u, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return storage.User{}, err
	}

	role := "user"
	if s.oidc.AdminGroup != "" && containsString(claims.Groups, s.oidc.AdminGroup) {
		role = "admin"
	}
	return s.store.CreateOIDCUser(ctx, claims.Email, oidcNoPassword, role, s.maxAccounts)
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
