package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// categoryView is the wire shape of a category (v4). sortIndex is the
// admin-controlled order.
type categoryView struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	SortIndex      int    `json:"sortIndex"`
	LayoutRow      int    `json:"layoutRow"`
	LayoutColOrder int    `json:"layoutColOrder"`
	LayoutWidthPct int    `json:"layoutWidthPct"`
}

func newCategoryView(c storage.Category) categoryView {
	return categoryView{
		ID:             c.ID,
		Name:           c.Name,
		SortIndex:      c.SortIndex,
		LayoutRow:      c.LayoutRow,
		LayoutColOrder: c.LayoutColOrder,
		LayoutWidthPct: c.LayoutWidthPct,
	}
}

// handleListCategories serves the categories in admin sort_index order (A1/A4).
// Session-gated like the rest of the catalog read; any logged-in user may read.
func (s *server) handleListCategories(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	cats, err := s.store.ListCategories(r.Context(), u.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]categoryView, 0, len(cats))
	for _, c := range cats {
		out = append(out, newCategoryView(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": out})
}

// handleCreateCategory adds a category to the caller's OWN dashboard (v9, A4 —
// no admin gate, per-user). A duplicate name (for this user) gets 409. The new
// category is appended last (sort_index max+1 among the user's categories).
func (s *server) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	c, err := s.store.CreateCategory(r.Context(), u.ID, in.Name)
	if errors.Is(err, storage.ErrNameTaken) {
		http.Error(w, "a category with that name already exists", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, newCategoryView(c))
}

// handleUpdateCategory renames one of the caller's OWN categories (v9, A4 — no
// admin gate, owner-scoped: another user's id → 404, D2/A14). A name collision
// (for this user) gets 409.
func (s *server) handleUpdateCategory(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	c, err := s.store.RenameCategory(r.Context(), r.PathValue("id"), u.ID, in.Name)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such category", http.StatusNotFound)
		return
	}
	if errors.Is(err, storage.ErrNameTaken) {
		http.Error(w, "a category with that name already exists", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, newCategoryView(c))
}

// handleSetCategoryOrder reorders the caller's OWN categories whole-array (v9,
// A4 — no admin gate, owner-scoped), the same contract as PUT /api/layout. An id
// not naming one of the caller's categories → 404. Success is 204.
func (s *server) handleSetCategoryOrder(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
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

	err := s.store.SetCategoryOrder(r.Context(), u.ID, in.Order)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such category in order", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteCategory deletes one of the caller's OWN categories (v9, A4 — no
// admin gate, owner-scoped: another user's row is never touched, A14).
// Idempotent: deleting an absent category is still 204. The FK is ON DELETE SET
// NULL, so the category's apps fall back to Uncategorized — none deleted.
func (s *server) handleDeleteCategory(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := s.store.DeleteCategory(r.Context(), r.PathValue("id"), u.ID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetCategoryLayout persists a batch of category layout assignments for
// the caller's OWN categories atomically (SPEC AC10 — all-or-nothing). An id not
// naming one of the caller's categories → 404 and the whole batch rolls back;
// no partial layout is ever stored. Success is 200.
func (s *server) handleSetCategoryLayout(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var in struct {
		Layout []struct {
			ID             string `json:"id"`
			LayoutRow      int    `json:"layoutRow"`
			LayoutColOrder int    `json:"layoutColOrder"`
			LayoutWidthPct int    `json:"layoutWidthPct"`
		} `json:"layout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	updates := make([]storage.CategoryLayout, 0, len(in.Layout))
	for _, l := range in.Layout {
		updates = append(updates, storage.CategoryLayout{
			ID:             l.ID,
			LayoutRow:      l.LayoutRow,
			LayoutColOrder: l.LayoutColOrder,
			LayoutWidthPct: l.LayoutWidthPct,
		})
	}

	err := s.store.SetCategoryLayout(r.Context(), u.ID, updates)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such category in layout", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
