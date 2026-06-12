package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// minPasswordLen is the minimum length enforced at registration (#12). Kept
// modest so it gates obviously-weak inputs without dictating a full policy.
const minPasswordLen = 8

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type userView struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	ThemePref string `json:"themePref"`
}

func newUserView(u storage.User) userView {
	return userView{ID: u.ID, Email: u.Email, Role: u.Role, ThemePref: u.ThemePref}
}

func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	c, ok := decodeCredentials(w, r)
	if !ok {
		return
	}

	ctx := r.Context()
	count, err := s.store.CountUsers(ctx)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// First registered user bootstraps as admin (spec Q2); after that,
	// self-registration is only allowed when registration is open. Any mode
	// other than RegistrationOpen fails closed (fail-safe; see #10).
	role := "user"
	if count == 0 {
		role = "admin"
	} else if s.registration != RegistrationOpen {
		http.Error(w, "registration is invite-only", http.StatusForbidden)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(c.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	u, err := s.store.CreateUser(ctx, c.Email, string(hash), role)
	if errors.Is(err, storage.ErrEmailTaken) {
		http.Error(w, "email already registered", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, newUserView(u))
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	c, ok := decodeCredentials(w, r)
	if !ok {
		return
	}

	u, err := s.store.UserByEmail(r.Context(), c.Email)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(c.Password)) != nil {
		http.Error(w, "invalid email or password", http.StatusUnauthorized)
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
	writeJSON(w, http.StatusOK, newUserView(u))
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Destroy(ck.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, newUserView(u))
}

// handlePatchMe updates the current user's own account fields. For v3 the only
// field is themePref (system|light|dark). Session-gated: 401 if not logged in.
// An unknown themePref value → 400, leaving the stored value unchanged. It
// writes only the current user's row — there is no path to another user's.
func (s *server) handlePatchMe(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var body struct {
		ThemePref string `json:"themePref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if !validThemePref(body.ThemePref) {
		http.Error(w, "themePref must be one of system, light, dark", http.StatusBadRequest)
		return
	}

	if err := s.store.SetThemePref(r.Context(), u.ID, body.ThemePref); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	u.ThemePref = body.ThemePref
	writeJSON(w, http.StatusOK, newUserView(u))
}

func validThemePref(v string) bool {
	return v == "system" || v == "light" || v == "dark"
}

func (s *server) currentUser(r *http.Request) (storage.User, bool) {
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return storage.User{}, false
	}
	userID, ok := s.sessions.UserID(ck.Value)
	if !ok {
		return storage.User{}, false
	}
	u, err := s.store.UserByID(r.Context(), userID)
	if err != nil {
		return storage.User{}, false
	}
	return u, true
}

func decodeCredentials(w http.ResponseWriter, r *http.Request) (credentials, bool) {
	var c credentials
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return c, false
	}
	c.Email = strings.TrimSpace(c.Email)
	if c.Email == "" || c.Password == "" {
		http.Error(w, "email and password are required", http.StatusBadRequest)
		return c, false
	}
	if _, err := mail.ParseAddress(c.Email); err != nil {
		http.Error(w, "invalid email address", http.StatusBadRequest)
		return c, false
	}
	if len(c.Password) < minPasswordLen {
		http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
		return c, false
	}
	return c, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
