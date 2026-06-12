package api

import (
	"encoding/json"
	"net/http"
)

// handleGetCollapsedCategories returns the current user's collapsed category-id
// set (v5). Session-gated; a user only reads their own set. Default is empty
// (everything expanded).
func (s *server) handleGetCollapsedCategories(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ids, err := s.store.CollapsedCategoryIDs(r.Context(), u.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"collapsed": ids})
}

// handleSetCollapsedCategories replaces the current user's collapsed set with
// exactly the ids in the body (whole-set PUT, like /api/layout). Session-gated;
// a user only writes their own set. Unknown/stale/malformed ids are silently
// dropped (the storage layer keeps only ids naming a live category), so this is
// never a 4xx for a category deleted between read and write. Success is 204.
func (s *server) handleSetCollapsedCategories(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var in struct {
		Collapsed []string `json:"collapsed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := s.store.SetCollapsedCategories(r.Context(), u.ID, in.Collapsed); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
