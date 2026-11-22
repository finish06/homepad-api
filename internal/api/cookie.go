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

// sessionCookie builds the homepad_session cookie. Every session cookie the
// server sets comes from here or clearedSessionCookie, so they all carry the
// same flags: always HttpOnly and SameSite=Lax, Secure from COOKIE_SECURE (see
// ParseCookieSecure). Max-Age matches the session's sliding lifetime, so the
// browser keeps the cookie exactly as long as the server keeps the session,
// and it is re-issued with a fresh Max-Age whenever the expiry slides (AC-010).
func (s *server) sessionCookie(token string, lifetime time.Duration) *http.Cookie {
	return s.baseSessionCookie(token, int(lifetime/time.Second))
}

// clearedSessionCookie is the cookie logout sets: empty and already expired.
func (s *server) clearedSessionCookie() *http.Cookie {
	return s.baseSessionCookie("", -1)
}

func (s *server) baseSessionCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

// startSession signs u in on this browser: it persists a new session with the
// role's lifetime and the request's User-Agent, sets the cookie, and then
// destroys any session the browser already carried, so a re-login replaces
// rather than orphans it. The cookie's Secure flag is the server's
// COOKIE_SECURE for every sign-in path. On failure it has written a 500 and
// returns false.
func (s *server) startSession(w http.ResponseWriter, r *http.Request, u storage.User) bool {
	if err := s.beginSession(w, r, u); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	return true
}

// beginSession is startSession without the error page: on failure it has
// logged and written nothing, so the OIDC callback can land the browser on
// the sign-in screen instead (SPEC-signin-hardening AC-002).
func (s *server) beginSession(w http.ResponseWriter, r *http.Request, u storage.User) error {
	ctx := r.Context()
	lifetime := session.Lifetime(u.Role)
	token, _, err := s.sessions.Create(ctx, u.ID, lifetime, r.UserAgent())
	if err != nil {
		log.Printf("session: create for user %s: %v", u.ID, err)
		return err
	}
	if old, err := r.Cookie(sessionCookie); err == nil && old.Value != "" && old.Value != token {
		if err := s.sessions.Destroy(ctx, old.Value); err != nil {
			// Best effort: the old session still expires on its own.
			log.Printf("session: destroy replaced session: %v", err)
		}
	}
	http.SetCookie(w, s.sessionCookie(token, lifetime))
	return nil
}
