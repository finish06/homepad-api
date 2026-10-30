package api

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// ParseCookieSecure resolves the session cookie's Secure attribute from the raw
// COOKIE_SECURE env value. It defaults to true (secure on) so production — which
// sets nothing and runs behind Pangolin TLS — is safe by default; unset, empty,
// or unparseable values all stay secure. The plain-HTTP nip.io staging/dev setup
// must explicitly set COOKIE_SECURE=false (or 0) to turn it off (#14).
func ParseCookieSecure(raw string) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return true
	}
	return b
}

// newSessionCookie builds the homepad_session cookie. It is always HttpOnly and
// SameSite=Lax; Secure is the caller's (see ParseCookieSecure). Max-Age matches
// the session's sliding lifetime, so the browser keeps the cookie exactly as
// long as the server keeps the session, and it is re-issued with a fresh
// Max-Age whenever the expiry slides (AC-010).
func newSessionCookie(token string, lifetime time.Duration, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(lifetime / time.Second),
	}
}

// startSession signs u in on this browser: it persists a new session with the
// role's lifetime and the request's User-Agent, sets the cookie, and then
// destroys any session the browser already carried, so a re-login replaces
// rather than orphans it. On failure it has written a 500 and returns false.
func (s *server) startSession(w http.ResponseWriter, r *http.Request, u storage.User, secure bool) bool {
	ctx := r.Context()
	lifetime := session.Lifetime(u.Role)
	token, _, err := s.sessions.Create(ctx, u.ID, lifetime, r.UserAgent())
	if err != nil {
		log.Printf("session: create for user %s: %v", u.ID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if old, err := r.Cookie(sessionCookie); err == nil && old.Value != "" && old.Value != token {
		if err := s.sessions.Destroy(ctx, old.Value); err != nil {
			// Best effort: the old session still expires on its own.
			log.Printf("session: destroy replaced session: %v", err)
		}
	}
	http.SetCookie(w, newSessionCookie(token, lifetime, secure))
	return true
}
