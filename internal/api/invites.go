package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// SPEC-household-basics §2.2 / §7 — invite links for adding an adult.
//
// An admin makes an invite with a name and a role (admin or user) and gets a
// one-time token, returned once. The web app puts it in a URL fragment
// (/#invite=…), so it never reaches an access log; the API only ever receives
// it in POST bodies. Only sha256(token) is stored. Lookup and accept are
// public, share one rate limit, and answer every dead token (unknown,
// malformed, expired, used, revoked) with the same 404.

// inviteTTL is how long an invite works (Q4: 7 days, fixed).
const inviteTTL = 7 * 24 * time.Hour

// InviteRetention is how long used, revoked and expired invites are kept for
// the audit trail before main's hourly sweep deletes them (§3.3).
const InviteRetention = 30 * 24 * time.Hour

// deadInvite is the one answer for every dead token (AC-012).
const deadInvite = "invite not found"

// inviteTokenLen is the token's length: 32 bytes, unpadded base64url.
var inviteTokenLen = base64.RawURLEncoding.EncodedLen(32)

type inviteView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Token is set only on the create response (AC-009).
	Token     string         `json:"token,omitempty"`
	CreatedBy *inviteCreator `json:"createdBy,omitempty"`
}

type inviteCreator struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// newInviteToken returns 32 CSPRNG bytes as unpadded base64url, and the
// SHA-256 that is stored in its place.
func newInviteToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashInviteToken(token), nil
}

func hashInviteToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// inviteTokenHash returns the stored form of a client-supplied token, or false
// when it can't be one of ours. A malformed token is then answered like any
// other dead one, without a database read.
func inviteTokenHash(token string) ([]byte, bool) {
	if len(token) != inviteTokenLen {
		return nil, false
	}
	if raw, err := base64.RawURLEncoding.DecodeString(token); err != nil || len(raw) != 32 {
		return nil, false
	}
	return hashInviteToken(token), true
}

// handleCreateInvite — POST /api/admin/invites {name, role} → 201 with the
// token, once (AC-008, AC-009). 409 at the account cap (AC-029).
func (s *server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Name *string `json:"name"`
		Role *string `json:"role"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.Name == nil {
		http.Error(w, kidNameRule, http.StatusBadRequest)
		return
	}
	name, ok := cleanKidName(*in.Name)
	if !ok {
		http.Error(w, kidNameRule, http.StatusBadRequest)
		return
	}
	// Only the two adult roles. A kid is never invited: kids are made in
	// Accounts, with no email or password (§3.1).
	if in.Role == nil || (*in.Role != "admin" && *in.Role != "user") {
		http.Error(w, "role must be admin or user", http.StatusBadRequest)
		return
	}
	token, hash, err := newInviteToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	inv, err := s.store.CreateInvite(r.Context(), hash, name, *in.Role, admin.ID, inviteTTL, s.maxAccounts)
	if errors.Is(err, storage.ErrAccountLimit) {
		s.refuseAtLimit(w, r)
		return
	}
	if err != nil {
		log.Printf("invite: create: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	log.Printf("invite: admin %s created invite %s role=%s expires=%s",
		admin.ID, inv.ID, inv.Role, inv.ExpiresAt.UTC().Format(time.RFC3339))
	writeJSON(w, http.StatusCreated, inviteView{
		ID: inv.ID, Name: inv.Name, Role: inv.Role, CreatedAt: inv.CreatedAt, ExpiresAt: inv.ExpiresAt, Token: token,
	})
}

// handleListInvites — GET /api/admin/invites: pending invites only, newest
// first, never with a token (AC-011).
func (s *server) handleListInvites(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	invs, err := s.store.PendingInvites(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]inviteView, 0, len(invs))
	for _, inv := range invs {
		out = append(out, inviteView{
			ID: inv.ID, Name: inv.Name, Role: inv.Role, CreatedAt: inv.CreatedAt, ExpiresAt: inv.ExpiresAt,
			CreatedBy: &inviteCreator{ID: inv.CreatedByID, Name: inv.CreatedByName},
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRevokeInvite — DELETE /api/admin/invites/{id} → 204; 404 when the
// invite isn't pending (AC-011).
func (s *server) handleRevokeInvite(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	inv, err := s.store.RevokeInvite(r.Context(), r.PathValue("id"))
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such pending invite", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	log.Printf("invite: admin %s revoked invite %s", admin.ID, inv.ID)
	w.WriteHeader(http.StatusNoContent)
}

// liveInvite resolves a client-supplied token to its pending invite. Every
// dead token gets the same 404, written here.
func (s *server) liveInvite(w http.ResponseWriter, r *http.Request, token string) ([]byte, storage.Invite, bool) {
	hash, ok := inviteTokenHash(token)
	if !ok {
		http.Error(w, deadInvite, http.StatusNotFound)
		return nil, storage.Invite{}, false
	}
	inv, err := s.store.LookupInvite(r.Context(), hash)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, deadInvite, http.StatusNotFound)
		return nil, storage.Invite{}, false
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, storage.Invite{}, false
	}
	return hash, inv, true
}

// handleLookupInvite — POST /api/invites/lookup {token} → {name, role,
// inviterName, expiresAt} (AC-012). Public and rate-limited.
func (s *server) handleLookupInvite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	_, inv, ok := s.liveInvite(w, r, in.Token)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Name        string    `json:"name"`
		Role        string    `json:"role"`
		InviterName string    `json:"inviterName"`
		ExpiresAt   time.Time `json:"expiresAt"`
	}{inv.Name, inv.Role, inv.CreatedByName, inv.ExpiresAt})
}

// handleAcceptInvite — POST /api/invites/accept {token, email, password,
// name?} → 201 with the /api/me body and an adult session (AC-013). The role
// is the invite's; any other field, role included, is a 400. A failure on
// the invitee's input leaves the invite usable (AC-014). Public and
// rate-limited.
func (s *server) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string  `json:"token"`
		Email    string  `json:"email"`
		Password string  `json:"password"`
		Name     *string `json:"name"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	email := strings.TrimSpace(in.Email)
	if email == "" || in.Password == "" {
		http.Error(w, "email and password are required", http.StatusBadRequest)
		return
	}
	if _, err := mail.ParseAddress(email); err != nil {
		http.Error(w, "invalid email address", http.StatusBadRequest)
		return
	}
	if len(in.Password) < minPasswordLen {
		http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
		return
	}
	name := ""
	if in.Name != nil {
		n, ok := cleanKidName(*in.Name)
		if !ok {
			http.Error(w, kidNameRule, http.StatusBadRequest)
			return
		}
		name = n
	}

	// Check the token before spending bcrypt on it.
	hash, _, ok := s.liveInvite(w, r, in.Token)
	if !ok {
		return
	}
	pw, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	u, inv, err := s.store.AcceptInvite(r.Context(), hash, email, string(pw), name)
	switch {
	case errors.Is(err, storage.ErrNotFound): // used or revoked meanwhile
		http.Error(w, deadInvite, http.StatusNotFound)
		return
	case errors.Is(err, storage.ErrEmailTaken):
		http.Error(w, "email already registered", http.StatusConflict)
		return
	case err != nil:
		log.Printf("invite: accept: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	log.Printf("invite: invite %s accepted, created user %s role=%s", inv.ID, u.ID, u.Role)
	if !s.startSession(w, r, u, s.cookieSecure) {
		return
	}
	writeJSON(w, http.StatusCreated, newUserView(u))
}

// refuseAtLimit answers 409 at HOMEPAD_MAX_ACCOUNTS and logs it (§3.5).
func (s *server) refuseAtLimit(w http.ResponseWriter, r *http.Request) {
	log.Printf("accounts: limit %d reached, refused %s %s", s.maxAccounts, r.Method, r.URL.Path)
	http.Error(w, accountLimitMessage(s.maxAccounts), http.StatusConflict)
}
