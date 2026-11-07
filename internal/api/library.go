package api

import (
	"encoding/json"
	"errors"
	"log"
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
	u, ok := s.currentUser(w, r)
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

// requireCatalogAdmin is requireAdmin for a catalog write (classCatalogWrite):
// it returns the shared catalog owner, whose rows the write must act on for
// every admin, not just the first (SPEC-household-basics AC-023, #87). The
// caller's own id is for the caller's own things only (favorites, layout,
// preferences). The caller is an admin, so an owner always exists; a failed
// lookup is a 500.
func (s *server) requireCatalogAdmin(w http.ResponseWriter, r *http.Request) (owner string, ok bool) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return "", false
	}
	owner, err := s.store.SharedCatalogOwnerID(r.Context())
	if err != nil {
		log.Printf("catalog: resolve owner: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return "", false
	}
	return owner, true
}

// handleListLibrary serves the full App Library to any authenticated user, in
// sort_index order, each offer tagged with whether the shared catalog already
// has a copy (A9; AC-024: the same hint for every caller).
func (s *server) handleListLibrary(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentUser(w, r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// With no admin there is no catalog, so nothing reads "added" ("" owner).
	owner, err := s.store.SharedCatalogOwnerID(r.Context())
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	offers, err := s.store.ListLibrary(r.Context(), owner)
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

// handleAddFromLibrary copies an offer into the shared catalog (A10) and returns
// the new serviceView. Admin-only under the shared catalog model (SPEC-245-224,
// #224): a non-admin session gets 403. The row is owned by the acting admin (the
// shared-catalog owner). Optional categoryId must be the caller's own (A11) else
// 400; absent/null lands Uncategorized (D4). Unknown offer id → 404. Adding twice
// is allowed and makes a second copy (D6).
func (s *server) handleAddFromLibrary(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.requireCatalogAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		CategoryID *string `json:"categoryId"`
	}
	// An empty body is valid (Uncategorized). Only reject malformed JSON.
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}

	sv, err := s.store.AddFromLibrary(r.Context(), owner, r.PathValue("id"), in.CategoryID)
	if errors.Is(err, storage.ErrCategoryNotFound) {
		http.Error(w, "no such category", http.StatusBadRequest)
		return
	}
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such library offer", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, serviceView{
		ID:              sv.ID,
		Slug:            sv.Slug,
		Name:            sv.Name,
		Description:     sv.Description,
		URL:             sv.URL,
		Icon:            sv.Icon,
		Status:          statusFor(s.poller.Snapshot(), sv.GatusKey),
		CategoryID:      sv.CategoryID,
		CategoryName:    sv.CategoryName,
		SourceLibraryID: sv.SourceLibraryID,
	})
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
