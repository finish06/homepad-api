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
	ID        string `json:"id"`
	Name      string `json:"name"`
	SortIndex int    `json:"sortIndex"`
}

func newCategoryView(c storage.Category) categoryView {
	return categoryView{ID: c.ID, Name: c.Name, SortIndex: c.SortIndex}
}

// handleListCategories serves the categories in admin sort_index order (A1/A4).
// Session-gated like the rest of the catalog read; any logged-in user may read.
func (s *server) handleListCategories(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentUser(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	cats, err := s.store.ListCategories(r.Context())
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

// handleCreateCategory lets an admin add a category (A1/A2). Non-admins get 403;
// a duplicate name gets 409. The new category is appended last (sort_index max+1).
func (s *server) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if u.Role != "admin" {
		http.Error(w, "admin role required", http.StatusForbidden)
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

	c, err := s.store.CreateCategory(r.Context(), in.Name)
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

// handleUpdateCategory lets an admin rename a category (A2/A3). Non-admins get
// 403; an unknown id gets 404; a name collision gets 409.
func (s *server) handleUpdateCategory(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if u.Role != "admin" {
		http.Error(w, "admin role required", http.StatusForbidden)
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

	c, err := s.store.RenameCategory(r.Context(), r.PathValue("id"), in.Name)
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

// handleSetCategoryOrder reorders categories whole-array (A2/A4), the same
// contract as PUT /api/layout. Non-admins get 403; success is 204.
func (s *server) handleSetCategoryOrder(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if u.Role != "admin" {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}

	var in struct {
		Order []string `json:"order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	err := s.store.SetCategoryOrder(r.Context(), in.Order)
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

// handleDeleteCategory lets an admin delete a category (A2/A7). Non-admins get
// 403. Idempotent: deleting an absent category is still 204. The FK is ON DELETE
// SET NULL, so the category's apps fall back to Uncategorized — none deleted.
func (s *server) handleDeleteCategory(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if u.Role != "admin" {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	if err := s.store.DeleteCategory(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
