package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// SPEC-kid-accounts §7 — the admin Accounts API. Every route here is
// registered admin-only in server.go: requireNotKid in front, requireAdmin in
// each handler. Managing adult accounts is out of scope (§10): adults are
// listed, and no route here can change, delete or sign in as one — every {id}
// is resolved with KidByID, which treats an adult's id as not found.

const (
	maxKidNameLen      = 40 // characters, not bytes (UX §3)
	defaultAvatarColor = "blue"
	maxAdminBody       = 64 * 1024
)

// avatarColors are the frontend's accent ids (src/theme/accent.ts); the UX
// offers six of them as swatches. Mirrors users_avatar_color_check.
var avatarColors = map[string]bool{
	"blue": true, "teal": true, "green": true, "yellow": true,
	"orange": true, "red": true, "pink": true, "purple": true,
}

// accountView is one row of GET /api/admin/accounts and the body of the kid
// create/update responses. The §7 fields are id, name, role and lastSeenAt;
// the rest are additive so the Accounts list can render "12 apps · 1 device"
// (UX §2) without a request per kid.
type accountView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
	// LastSeenAt is the most recent use of any live session; null if none.
	LastSeenAt  *time.Time `json:"lastSeenAt"`
	Email       string     `json:"email,omitempty"` // adults only
	AvatarColor string     `json:"avatarColor"`
	AppCount    *int       `json:"appCount,omitempty"`    // kids only
	DeviceCount *int       `json:"deviceCount,omitempty"` // kids only
}

// deviceView is one signed-in device of a kid (§7). ID is the session's
// opaque handle: never the token, never its hash.
type deviceView struct {
	ID         string    `json:"id"`
	UserAgent  string    `json:"userAgent"`
	CreatedAt  time.Time `json:"createdAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
}

func (s *server) newAccountView(r *http.Request, u storage.User, appCount int) (accountView, error) {
	v := accountView{ID: u.ID, Name: u.DisplayName, Role: u.Role, AvatarColor: u.AvatarColor}
	if storage.IsAdultRole(u.Role) {
		v.Email = u.Email
		if v.Name == "" {
			v.Name = u.Email
		}
	}
	sessions, err := s.sessions.ListForUser(r.Context(), u.ID)
	if err != nil {
		return accountView{}, err
	}
	if len(sessions) > 0 {
		last := sessions[0].LastSeenAt // ListForUser is most recently seen first
		v.LastSeenAt = &last
	}
	if !storage.IsAdultRole(u.Role) {
		apps, devices := appCount, len(sessions)
		v.AppCount, v.DeviceCount = &apps, &devices
	}
	return v, nil
}

// decodeStrict decodes a JSON object into dst, refusing unknown fields, so a
// field this API does not support (e.g. a kid's email) is a 400, not ignored.
func decodeStrict(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return false
	}
	return true
}

// cleanKidName trims and validates a kid name: 1–40 characters, no control
// characters.
func cleanKidName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if !utf8.ValidString(name) {
		return "", false
	}
	n := utf8.RuneCountInString(name)
	if n < 1 || n > maxKidNameLen {
		return "", false
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return "", false
		}
	}
	return name, true
}

const kidNameRule = "name must be 1-40 characters with no control characters"

// kidFor resolves the {id} path value to a kid, writing 404 (not a kid —
// including an adult's id) or 500 itself.
func (s *server) kidFor(w http.ResponseWriter, r *http.Request) (storage.User, bool) {
	kid, err := s.store.KidByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such kid account", http.StatusNotFound)
		return storage.User{}, false
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return storage.User{}, false
	}
	return kid, true
}

// handleListAccounts — GET /api/admin/accounts: every account (§2.3).
func (s *server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	accts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]accountView, 0, len(accts))
	for _, a := range accts {
		v, err := s.newAccountView(r, a.User, a.AppCount)
		if err != nil {
			log.Printf("accounts: list sessions: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateKid — POST /api/admin/kids {name, avatarColor?} → 201 (AC-001).
func (s *server) handleCreateKid(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var in struct {
		Name        *string `json:"name"`
		AvatarColor *string `json:"avatarColor"`
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
	color := defaultAvatarColor
	if in.AvatarColor != nil {
		if !avatarColors[*in.AvatarColor] {
			http.Error(w, "avatarColor must be an accent id", http.StatusBadRequest)
			return
		}
		color = *in.AvatarColor
	}
	kid, err := s.store.CreateKid(r.Context(), name, color)
	if errors.Is(err, storage.ErrKidNameTaken) {
		http.Error(w, "a kid with that name already exists", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.writeAccount(w, r, http.StatusCreated, kid)
}

// handleUpdateKid — PATCH /api/admin/kids/{id} {name?, avatarColor?}.
//
// §7 also lists `password?`. It is refused (400): a kid has no email, so no
// sign-in path could use a password, and kids sign in only through
// sign-in-here. Adding kid passwords needs its own sign-in design first.
func (s *server) handleUpdateKid(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var in struct {
		Name        *string          `json:"name"`
		AvatarColor *string          `json:"avatarColor"`
		Password    *json.RawMessage `json:"password"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.Password != nil {
		http.Error(w, "kid accounts do not have passwords; use sign-in-here", http.StatusBadRequest)
		return
	}
	if in.Name == nil && in.AvatarColor == nil {
		http.Error(w, "body must include name or avatarColor", http.StatusBadRequest)
		return
	}
	if in.Name != nil {
		name, ok := cleanKidName(*in.Name)
		if !ok {
			http.Error(w, kidNameRule, http.StatusBadRequest)
			return
		}
		in.Name = &name
	}
	if in.AvatarColor != nil && !avatarColors[*in.AvatarColor] {
		http.Error(w, "avatarColor must be an accent id", http.StatusBadRequest)
		return
	}
	kid, err := s.store.UpdateKid(r.Context(), r.PathValue("id"), in.Name, in.AvatarColor)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such kid account", http.StatusNotFound)
		return
	}
	if errors.Is(err, storage.ErrKidNameTaken) {
		http.Error(w, "a kid with that name already exists", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.writeAccount(w, r, http.StatusOK, kid)
}

func (s *server) writeAccount(w http.ResponseWriter, r *http.Request, status int, kid storage.User) {
	ids, err := s.store.KidServiceIDs(r.Context(), kid.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	v, err := s.newAccountView(r, kid, len(ids))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, status, v)
}

// handleDeleteKid — DELETE /api/admin/kids/{id} → 204. Sessions, favorites,
// preferences and the allow-list go with it (AC-014).
func (s *server) handleDeleteKid(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	err := s.store.DeleteKid(r.Context(), r.PathValue("id"))
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such kid account", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGetKidServices — GET /api/admin/kids/{id}/services → {serviceIds}.
func (s *server) handleGetKidServices(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	kid, ok := s.kidFor(w, r)
	if !ok {
		return
	}
	ids, err := s.store.KidServiceIDs(r.Context(), kid.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"serviceIds": ids})
}

// handleSetKidServices — PUT /api/admin/kids/{id}/services {serviceIds} → 204.
// A whole-set replace (like PUT /api/layout); an id outside the shared catalog
// is a 400 and changes nothing (AC-002). serviceIds must be present: a missing
// or null list is a 400, not "clear everything".
func (s *server) handleSetKidServices(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var in struct {
		ServiceIDs *[]string `json:"serviceIds"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.ServiceIDs == nil {
		http.Error(w, "serviceIds is required (an array; [] for none)", http.StatusBadRequest)
		return
	}
	err := s.store.SetKidServices(r.Context(), r.PathValue("id"), *in.ServiceIDs)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such kid account", http.StatusNotFound)
		return
	}
	if errors.Is(err, storage.ErrUnknownService) {
		http.Error(w, "serviceIds names a service that is not in the catalog", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListKidSessions — GET /api/admin/kids/{id}/sessions → the kid's
// signed-in devices (AC-012). Only the handle identifies a session.
func (s *server) handleListKidSessions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	kid, ok := s.kidFor(w, r)
	if !ok {
		return
	}
	sessions, err := s.sessions.ListForUser(r.Context(), kid.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]deviceView, 0, len(sessions))
	for _, ss := range sessions {
		out = append(out, deviceView{ID: ss.Handle, UserAgent: ss.UserAgent, CreatedAt: ss.CreatedAt, LastSeenAt: ss.LastSeenAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRevokeKidSession — DELETE /api/admin/kids/{id}/sessions/{sid} → 204.
// The revoke is scoped to the kid in the path, so a handle of any other
// account is a 404. The revoked device gets 401 on its next request (AC-012).
func (s *server) handleRevokeKidSession(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	kid, ok := s.kidFor(w, r)
	if !ok {
		return
	}
	err := s.sessions.RevokeByHandle(r.Context(), kid.ID, r.PathValue("sid"))
	if errors.Is(err, session.ErrNotFound) {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSignInHere — POST /api/admin/kids/{id}/sign-in-here → 204 (AC-009,
// §2.2). It needs an admin session; it ends that session and sets a kid
// session cookie (180-day Max-Age) in the same response, so the device swaps
// from the admin to the kid. The swap is one-way: nothing here can turn a kid
// session back into an admin one.
//
// The admin's session is destroyed FIRST, and a failure stops the swap: the
// device must never end up holding a kid cookie while the admin's session
// stays valid server-side. (startSession, used by login, does the reverse —
// create, then best-effort destroy — which is fine for a re-login but not for
// this hand-over.)
func (s *server) handleSignInHere(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	kid, ok := s.kidFor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	ck, err := r.Cookie(sessionCookie)
	if err != nil { // requireAdmin passed, so the cookie is there
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := s.sessions.Destroy(ctx, ck.Value); err != nil {
		log.Printf("sign-in-here: end admin session: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	lifetime := session.Lifetime(kid.Role)
	token, _, err := s.sessions.Create(ctx, kid.ID, lifetime, r.UserAgent())
	if err != nil {
		// The admin is already signed out here; clear the cookie so the
		// device shows the login screen rather than a dead session.
		log.Printf("sign-in-here: create kid session: %v", err)
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
			Secure: s.cookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	log.Printf("sign-in-here: admin %s signed a device in as kid %s", admin.ID, kid.ID)
	http.SetCookie(w, newSessionCookie(token, lifetime, s.cookieSecure))
	w.WriteHeader(http.StatusNoContent)
}
