package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// categoryView is the wire shape of a category (v4). sortIndex is the
// admin-controlled order; gridWidth is the App Grid box width 1–8 (SPEC-app-grid).
type categoryView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SortIndex int    `json:"sortIndex"`
	GridWidth int    `json:"gridWidth"`
}

func newCategoryView(c storage.Category) categoryView {
	return categoryView{ID: c.ID, Name: c.Name, SortIndex: c.SortIndex, GridWidth: c.GridWidth}
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

// handleCreateCategory adds a category to the App Grid (issue #224, SPEC-app-grid
// §3A — "+Add box → POST /api/categories — Admin-only"). Creation is admin-gated:
// a non-admin session gets 403 (the frontend hides +Add, but the server must not
// trust the client gate). A duplicate name gets 409; the new category is appended
// last (sort_index max+1). This supersedes the v9/A4 per-user create model.
func (s *server) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
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

// handleUpdateCategory updates one of the caller's OWN categories (v9, A4 — no
// admin gate, owner-scoped: another user's id → 404, D2/A14). Two independently
// optional fields: `name` (rename; a name collision → 409) and `gridWidth` (the
// App Grid box width, 1–8, SPEC-app-grid §3B). A gridWidth-only PATCH must not
// require a name, and vice-versa; when both are present, rename then set width.
// At least one must be present.
func (s *server) handleUpdateCategory(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var in struct {
		Name      *string `json:"name"`
		GridWidth *int    `json:"gridWidth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if in.Name == nil && in.GridWidth == nil {
		http.Error(w, "name or gridWidth is required", http.StatusBadRequest)
		return
	}
	if in.GridWidth != nil && (*in.GridWidth < 1 || *in.GridWidth > 8) {
		http.Error(w, "gridWidth must be between 1 and 8", http.StatusBadRequest)
		return
	}

	id := r.PathValue("id")
	var c storage.Category
	var err error

	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		c, err = s.store.RenameCategory(r.Context(), id, u.ID, name)
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
	}

	if in.GridWidth != nil {
		c, err = s.store.SetCategoryWidth(r.Context(), id, u.ID, *in.GridWidth)
		if errors.Is(err, storage.ErrNotFound) {
			http.Error(w, "no such category", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
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
