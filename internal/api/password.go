package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// SPEC-password-reset-link — an admin-made, one-time reset link, and
// changing your own password.
//
// The link follows the invite rules (SPEC-household-basics §3.2): 32 CSPRNG
// bytes as unpadded base64url, only sha256(token) stored, the token returned
// once and carried in a URL fragment (/#reset=…), so the API only ever gets it
// in POST bodies. Lookup and accept are public, share the invite routes' rate
// limit, and answer every dead link with the same 404. No line logged here
// carries a token, a hash, an email or a password (§3.4).

// resetTTL is how long a reset link works (Q1: 24 hours).
const resetTTL = 24 * time.Hour

// ResetRetention is how long used, revoked and expired links are kept for the
// audit trail before main's hourly sweep deletes them (§6).
const ResetRetention = 30 * 24 * time.Hour

// deadReset is the one answer for every dead reset link (AC-006).
const deadReset = "reset link not found"

// Change password's guard against a borrowed, signed-in device (§3.3):
// passwordFailMax wrong current passwords within passwordFailWindow lock the
// route for that account for passwordLockFor.
const (
	passwordFailMax    = 5
	passwordFailWindow = 15 * time.Minute
	passwordLockFor    = 15 * time.Minute
	// passwordLogEvery throttles the wrong-password audit line per account.
	passwordLogEvery = time.Minute
)

// passwordError is the {"error": code} body of the refusals the web app tells
// apart: self, sso_account, wrong_password.
type passwordError struct {
	Error string `json:"error"`
}

// handleCreateResetLink — POST /api/admin/accounts/{id}/reset-link → 201
// {id, expiresAt, token}, the token only here (AC-001). 404 for anything but
// an adult; 409 self (use Change password) or sso_account (Q4).
func (s *server) handleCreateResetLink(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	targetID := r.PathValue("id")
	if targetID == admin.ID {
		writeJSON(w, http.StatusConflict, passwordError{"self"})
		return
	}
	token, link, err := makeResetLink(r.Context(), s.store, targetID, admin.ID)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		http.Error(w, "no such adult account", http.StatusNotFound)
		return
	case errors.Is(err, storage.ErrSSOAccount):
		writeJSON(w, http.StatusConflict, passwordError{"sso_account"})
		return
	case errors.Is(err, storage.ErrNotAdmin):
		// Demoted or removed by another admin after this request's check.
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	case err != nil:
		log.Printf("reset: create: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		ID        string    `json:"id"`
		ExpiresAt time.Time `json:"expiresAt"`
		Token     string    `json:"token"`
	}{link.ID, link.ExpiresAt, token})
}

// makeResetLink makes a link for userID on behalf of actorID ("" is the
// CLI), and writes the §3.4 lines: one per earlier link it revoked (Q6), then
// the create line. The token is returned and never logged.
func makeResetLink(ctx context.Context, store *storage.Store, userID, actorID string) (string, storage.PasswordReset, error) {
	token, hash, err := newInviteToken()
	if err != nil {
		return "", storage.PasswordReset{}, err
	}
	link, revoked, err := store.CreatePasswordReset(ctx, hash, userID, actorID, resetTTL)
	if err != nil {
		return "", storage.PasswordReset{}, err
	}
	actor := actorID
	if actor == "" {
		actor = "cli"
	}
	for _, id := range revoked {
		log.Printf("reset: link %s revoked by a newer link", id)
	}
	log.Printf("reset: admin %s created reset link %s for user %s expires=%s",
		actor, link.ID, link.UserID, link.ExpiresAt.UTC().Format(time.RFC3339))
	return token, link, nil
}

// liveReset resolves a client-supplied token to its live link. Every dead
// token gets the same 404, written here; a malformed one costs no database
// read.
func (s *server) liveReset(w http.ResponseWriter, r *http.Request, token string) ([]byte, storage.PasswordReset, bool) {
	hash, ok := inviteTokenHash(token)
	if !ok {
		http.Error(w, deadReset, http.StatusNotFound)
		return nil, storage.PasswordReset{}, false
	}
	link, err := s.store.LookupPasswordReset(r.Context(), hash)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, deadReset, http.StatusNotFound)
		return nil, storage.PasswordReset{}, false
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, storage.PasswordReset{}, false
	}
	return hash, link, true
}

// handleLookupReset — POST /api/password-resets/lookup {token} → {email,
// name, expiresAt} (AC-006). Looking up doesn't use the link. Public and
// rate-limited.
func (s *server) handleLookupReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	_, link, ok := s.liveReset(w, r, in.Token)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Email     string    `json:"email"`
		Name      string    `json:"name"`
		ExpiresAt time.Time `json:"expiresAt"`
	}{link.Email, link.Name, link.ExpiresAt})
}

// newPasswordProblem is the 400 for a new password that breaks the rules
// (at least 8 characters, at most 72 bytes), or "" when it's fine.
func newPasswordProblem(pw string) string {
	switch {
	case len(pw) < minPasswordLen:
		return "password must be at least 8 characters"
	case passwordTooLong(pw):
		return passwordTooLongMsg
	}
	return ""
}

// handleAcceptReset — POST /api/password-resets/accept {token, password} →
// 200 with the /api/me body and a new session (AC-007). It sets the password,
// marks the link used, ends every session of the account (Q2, AC-008) and
// then signs this device in. A refusal on the input leaves the link usable
// (AC-009). Public and rate-limited.
func (s *server) handleAcceptReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string  `json:"token"`
		Password *string `json:"password"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.Password == nil {
		http.Error(w, "password is required", http.StatusBadRequest)
		return
	}
	if msg := newPasswordProblem(*in.Password); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	// Check the token before spending bcrypt on it.
	hash, _, ok := s.liveReset(w, r, in.Token)
	if !ok {
		return
	}
	pw, err := bcrypt.GenerateFromPassword([]byte(*in.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	u, link, err := s.store.UsePasswordReset(ctx, hash, string(pw))
	if errors.Is(err, storage.ErrNotFound) { // used, revoked or expired meanwhile
		http.Error(w, deadReset, http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("reset: accept: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	ended, err := s.sessions.DestroyAllForUser(ctx, u.ID, "")
	if err != nil {
		// The password is set and the link used; say so, and fail loudly so
		// nobody mistakes the old sessions for ended ones.
		log.Printf("reset: link %s used, user %s password set, ending sessions failed: %v", link.ID, u.ID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	log.Printf("reset: link %s used, user %s password set, %d sessions ended", link.ID, u.ID, ended)
	if !s.startSession(w, r, u) {
		return
	}
	writeJSON(w, http.StatusOK, newUserView(u))
}

// handleChangePassword — POST /api/me/password {currentPassword,
// newPassword} → 204 (AC-013). Every other session of the account ends; this
// one stays (Q3). A live reset link for the account is revoked. 403 wrong_password changes nothing and signs nobody out;
// 409 sso_account; 400 for the rules; 429 after passwordFailMax wrong
// current passwords (§3.3). A kid never gets here (classKidForbidden).
func (s *server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(w, r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var in struct {
		CurrentPassword *string `json:"currentPassword"`
		NewPassword     *string `json:"newPassword"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.CurrentPassword == nil || in.NewPassword == nil {
		http.Error(w, "currentPassword and newPassword are required", http.StatusBadRequest)
		return
	}
	if !u.HasLocalPassword() {
		writeJSON(w, http.StatusConflict, passwordError{"sso_account"})
		return
	}
	if wait := s.passwordFails.locked(u.ID); wait > 0 {
		w.Header().Set("Retry-After", retryAfterSeconds(wait))
		http.Error(w, "too many wrong passwords", http.StatusTooManyRequests)
		return
	}
	if msg := newPasswordProblem(*in.NewPassword); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if !s.passwordMatches(u, *in.CurrentPassword) {
		if s.passwordFails.fail(u.ID) {
			log.Printf("password: user %s wrong current password", u.ID)
		}
		writeJSON(w, http.StatusForbidden, passwordError{"wrong_password"})
		return
	}
	s.passwordFails.clear(u.ID)

	pw, err := bcrypt.GenerateFromPassword([]byte(*in.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	revoked, err := s.store.SetPasswordHash(ctx, u.ID, string(pw))
	if err != nil {
		log.Printf("password: set for user %s: %v", u.ID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, id := range revoked {
		log.Printf("reset: link %s revoked by a password change", id)
	}
	keep := ""
	if ck, err := r.Cookie(sessionCookie); err == nil {
		keep = ck.Value
	}
	ended, err := s.sessions.DestroyAllForUser(ctx, u.ID, keep)
	if err != nil {
		log.Printf("password: user %s changed their password, ending other sessions failed: %v", u.ID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	log.Printf("password: user %s changed their password, %d other sessions ended", u.ID, ended)
	w.WriteHeader(http.StatusNoContent)
}

// passwordMatches runs one bcrypt compare of password against u's hash. A
// password over 72 bytes never matches: bcrypt would ignore the extra bytes.
func (s *server) passwordMatches(u storage.User, password string) bool {
	compare := s.comparePassword
	if compare == nil {
		compare = bcrypt.CompareHashAndPassword
	}
	if passwordTooLong(password) || !isBcryptHash([]byte(u.PasswordHash)) {
		return false
	}
	return compare([]byte(u.PasswordHash), []byte(password)) == nil
}

// failureLimiter counts wrong current passwords per account (§3.3). It is
// per process, like the invite bucket.
type failureLimiter struct {
	mu    sync.Mutex
	now   func() time.Time
	byKey map[string]*failures
}

type failures struct {
	at          []time.Time // wrong tries inside the window, oldest first
	lockedUntil time.Time
	loggedAt    time.Time
}

func newFailureLimiter(now func() time.Time) *failureLimiter {
	if now == nil {
		now = time.Now
	}
	return &failureLimiter{now: now, byKey: map[string]*failures{}}
}

// locked returns how long key is still locked out, or 0.
func (l *failureLimiter) locked(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.byKey[key]
	if f == nil {
		return 0
	}
	if wait := f.lockedUntil.Sub(l.now()); wait > 0 {
		return wait
	}
	return 0
}

// fail records one wrong try for key, locking it at passwordFailMax inside
// the window. It reports whether this try should be logged (at most one line
// per passwordLogEvery).
func (l *failureLimiter) fail(key string) (logIt bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	f := l.byKey[key]
	if f == nil {
		f = &failures{}
		l.byKey[key] = f
	}
	kept := f.at[:0]
	for _, t := range f.at {
		if now.Sub(t) < passwordFailWindow {
			kept = append(kept, t)
		}
	}
	f.at = append(kept, now)
	if len(f.at) >= passwordFailMax {
		f.lockedUntil = now.Add(passwordLockFor)
		f.at = nil
	}
	if f.loggedAt.IsZero() || now.Sub(f.loggedAt) >= passwordLogEvery {
		f.loggedAt = now
		return true
	}
	return false
}

// clear forgets key's wrong tries after a right one.
func (l *failureLimiter) clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byKey, key)
}

// ResetLinkCommand is `homepad-api reset-link <email>` (Q5, AC-016): it makes
// a one-time link for the adult with that email and prints the /#reset= path
// and its expiry to stdout, the only place the token ever goes. Refusals go
// to stderr: exit 1 for an email that names no adult with a password, 2 for
// a usage error.
func ResetLinkCommand(ctx context.Context, store *storage.Store, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		fmt.Fprintln(stderr, "usage: homepad-api reset-link <email>")
		return 2
	}
	u, err := store.UserByEmail(ctx, strings.TrimSpace(args[0]))
	if errors.Is(err, storage.ErrNotFound) {
		fmt.Fprintln(stderr, "No adult account uses that email. Kids have no password to reset.")
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "Couldn't read the accounts: %v\n", err)
		return 1
	}
	token, link, err := makeResetLink(ctx, store, u.ID, "")
	switch {
	case errors.Is(err, storage.ErrSSOAccount):
		fmt.Fprintln(stderr, "That account signs in with single sign-on only, so it has no password to reset.")
		return 1
	case errors.Is(err, storage.ErrNotFound):
		fmt.Fprintln(stderr, "No adult account uses that email. Kids have no password to reset.")
		return 1
	case err != nil:
		fmt.Fprintf(stderr, "Couldn't make the link: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "/#reset=%s\n", token)
	fmt.Fprintf(stdout, "It works once, until %s. Put your Homepad address in front of it.\n",
		link.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"))
	return 0
}
