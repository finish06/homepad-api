package api

import (
	"errors"
	"log"
	"net/http"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// SPEC-household-manage-adults §7 — an admin changes an adult's role or
// removes an adult. Both routes are admin-only (requireNotKid in front,
// requireAdmin here), resolve {id} to adults only (a kid's id is a 404: kids
// keep /api/admin/kids/{id}), refuse the admin's own account (409 self, Q1)
// and refuse a change that would leave no admin (409 last_admin). The rules
// are enforced in storage, under the account lock.

const (
	patchAdultRoute  = "PATCH /api/admin/accounts/{id}"
	removeAdultRoute = "DELETE /api/admin/accounts/{id}"
)

// refuseAdultChange writes the answer for a storage refusal, or 500. The 409
// bodies are JSON so the web app can pick its line without parsing text.
func (s *server) refuseAdultChange(w http.ResponseWriter, admin storage.User, route string, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		http.Error(w, "no such adult account", http.StatusNotFound)
	case errors.Is(err, storage.ErrSelf):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "self"})
	case errors.Is(err, storage.ErrLastAdmin):
		log.Printf("accounts: admin %s refused: would leave no admin (%s)", admin.ID, route)
		writeJSON(w, http.StatusConflict, map[string]string{"error": "last_admin"})
	case errors.Is(err, storage.ErrNotAdmin):
		// Demoted by another admin after this request passed its check.
		http.Error(w, "admin role required", http.StatusForbidden)
	default:
		log.Printf("accounts: %s: %v", route, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// handlePatchAdult — PATCH /api/admin/accounts/{id} {role} → 200 with the
// account row (AC-006). role is "admin" or "user"; any other value, any other
// field or no body is 400. A real change ends every session of that account
// (Q2, AC-008); setting the role it already has changes nothing.
func (s *server) handlePatchAdult(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Role *string `json:"role"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.Role == nil || (*in.Role != "admin" && *in.Role != "user") {
		http.Error(w, `role must be "admin" or "user"`, http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	old, changed, revokedResets, err := s.store.SetAdultRole(ctx, admin.ID, r.PathValue("id"), *in.Role)
	if err != nil {
		s.refuseAdultChange(w, admin, patchAdultRoute, err)
		return
	}
	if changed {
		log.Printf("accounts: admin %s changed role of %s from %s to %s", admin.ID, old.ID, old.Role, *in.Role)
		if revokedResets > 0 {
			log.Printf("reset: revoked %d live reset links made by demoted admin %s", revokedResets, old.ID)
		}
		if _, err := s.sessions.DestroyAllForUser(ctx, old.ID, ""); err != nil {
			// The role is changed and read on every request, so the old
			// sessions already act with the new role; say so loudly anyway.
			log.Printf("accounts: end sessions of %s after a role change: %v", old.ID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	u := old
	u.Role = *in.Role
	v, err := s.newAccountView(r, u, 0)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleRemoveAdult — DELETE /api/admin/accounts/{id} → 204 (AC-001). Their
// sessions end with the row (FK cascade, AC-007) and their pending invites are
// revoked (Q3, AC-004), and so are the live reset links they made. The shared
// catalog is untouched.
func (s *server) handleRemoveAdult(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	removed, revoked, revokedResets, err := s.store.RemoveAdult(r.Context(), admin.ID, r.PathValue("id"))
	if err != nil {
		s.refuseAdultChange(w, admin, removeAdultRoute, err)
		return
	}
	log.Printf("accounts: admin %s removed adult %s role=%s", admin.ID, removed.ID, removed.Role)
	if revoked > 0 {
		log.Printf("invite: revoked %d pending invites made by removed adult %s", revoked, removed.ID)
	}
	if revokedResets > 0 {
		log.Printf("reset: revoked %d live reset links made by removed adult %s", revokedResets, removed.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}
