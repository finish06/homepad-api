package api

import (
	"errors"
	"net/http"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// handleAddFavorite marks a service as a favorite for the current user (A5).
// Idempotent — re-marking returns 204. Unknown service id returns 404.
func (s *server) handleAddFavorite(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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

// handleRemoveFavorite unmarks a favorite for the current user (A5). Idempotent.
func (s *server) handleRemoveFavorite(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := s.store.RemoveFavorite(r.Context(), u.ID, r.PathValue("id")); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
