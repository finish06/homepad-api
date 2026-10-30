package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// handleAddFavorite marks a service as a favorite for the current user (A5).
// Idempotent — re-marking returns 204. Unknown service id returns 404, and so
// does a service off a kid's allow-list (AC-006), with the same body.
func (s *server) handleAddFavorite(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(w, r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.serviceVisible(w, r, u, r.PathValue("id"), "no such service") {
		return
	}
	err := s.store.AddFavorite(r.Context(), u.ID, r.PathValue("id"))
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such service", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRemoveFavorite unmarks a favorite for the current user (A5). Idempotent
// for adults; for a kid, a service off its allow-list is 404 (AC-006).
func (s *server) handleRemoveFavorite(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(w, r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.serviceVisible(w, r, u, r.PathValue("id"), "no such service") {
		return
	}
	if err := s.store.RemoveFavorite(r.Context(), u.ID, r.PathValue("id")); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUpdateLayout saves the current user's personal catalog sort order (A5).
// Body: {"order": ["<service-id>", ...]}, position 0 first. Replaces any prior
// order. An id that names no service returns 404; the order then persists into
// GET /api/services for that user across sessions.
func (s *server) handleUpdateLayout(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(w, r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var in struct {
		Order []string `json:"order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	err := s.store.SetLayout(r.Context(), u.ID, in.Order)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such service in order", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
