package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// libraryOfferView is one App Library offer as returned to a browsing user
// (v9.2). `added` is the per-user hint (does the caller already hold a copy);
// it never blocks a re-add (D6).
type libraryOfferView struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	URL               string `json:"url"`
	Icon              string `json:"icon"`
	Description       string `json:"description"`
	SuggestedCategory string `json:"suggestedCategory"`
	SortIndex         int    `json:"sortIndex"`
	Added             bool   `json:"added"`
}

func offerView(la storage.LibraryApp, added bool) libraryOfferView {
	return libraryOfferView{
		ID:                la.ID,
		Name:              la.Name,
		URL:               la.URL,
		Icon:              la.Icon,
		Description:       la.Description,
		SuggestedCategory: la.SuggestedCategory,
		SortIndex:         la.SortIndex,
		Added:             added,
	}
}

// requireAdmin resolves the caller and enforces the admin role on the library
// curation routes (D10). It writes the 401/403 itself and returns ok=false so
// the handler can just return. 403 (not 404) is the contract for these shared,
// admin-scoped routes — distinct from the per-user 404 of Invariant 2.
func (s *server) requireAdmin(w http.ResponseWriter, r *http.Request) (storage.User, bool) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return storage.User{}, false
	}
	if u.Role != "admin" {
		http.Error(w, "admin role required", http.StatusForbidden)
		return storage.User{}, false
	}
	return u, true
}

// handleListLibrary serves the full App Library to any authenticated user, in
// sort_index order, each offer tagged with the caller's `added` hint (A9).
func (s *server) handleListLibrary(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	offers, err := s.store.ListLibrary(r.Context(), u.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]libraryOfferView, 0, len(offers))
	for _, o := range offers {
		out = append(out, offerView(o.LibraryApp, o.Added))
	}
	writeJSON(w, http.StatusOK, map[string]any{"library": out})
}

// handleCreateLibraryApp adds a new offer (admin only, A8); it appends at the
// end of the browse order.
func (s *server) handleCreateLibraryApp(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var in struct {
		Name              string `json:"name"`
		URL               string `json:"url"`
		Icon              string `json:"icon"`
		Description       string `json:"description"`
		SuggestedCategory string `json:"suggestedCategory"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.URL = strings.TrimSpace(in.URL)
	if in.Name == "" || in.URL == "" {
		http.Error(w, "name and url are required", http.StatusBadRequest)
		return
	}
	la, err := s.store.CreateLibraryApp(r.Context(), storage.LibraryApp{
		Name:              in.Name,
		URL:               in.URL,
		Icon:              in.Icon,
		Description:       in.Description,
		SuggestedCategory: in.SuggestedCategory,
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, offerView(la, false))
}

// handleUpdateLibraryApp edits an offer (admin only, A8). Only present fields
// change. Editing does NOT propagate to existing copies (C1). Unknown id → 404.
func (s *server) handleUpdateLibraryApp(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var in struct {
		Name              *string `json:"name"`
		URL               *string `json:"url"`
		Icon              *string `json:"icon"`
		Description       *string `json:"description"`
		SuggestedCategory *string `json:"suggestedCategory"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	la, err := s.store.UpdateLibraryApp(r.Context(), r.PathValue("id"), storage.LibraryAppUpdate{
		Name:              in.Name,
		URL:               in.URL,
		Icon:              in.Icon,
		Description:       in.Description,
		SuggestedCategory: in.SuggestedCategory,
	})
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such library offer", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, offerView(la, false))
}

// handleSetLibraryOrder reorders the whole library by position (admin only, A8);
// the v4 reorder contract. An unknown id leaves the prior order intact → 404.
func (s *server) handleSetLibraryOrder(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var in struct {
		Order []string `json:"order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	err := s.store.SetLibraryOrder(r.Context(), in.Order)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such library offer", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteLibraryApp removes an offer (admin only, A8). Idempotent. Existing
// copies are untouched; their source_library_id goes NULL via the FK (C1/OQ5).
func (s *server) handleDeleteLibraryApp(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if err := s.store.DeleteLibraryApp(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
