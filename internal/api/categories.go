package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// categoryView is the wire shape of a category (v4). sortIndex is the
// admin-controlled order; gridWidth is the App Grid box's 12-column SPAN — 3, 4, 6
// or 12 (SPEC-app-grid §10.4; was a 1–8 tile count before migration 0013).
type categoryView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SortIndex int    `json:"sortIndex"`
	GridWidth int    `json:"gridWidth"`
}

// validGridSpan — the four legal 12-column spans (quarter, third, half, full).
// Mirrors the DB CHECK from migration 0013.
func validGridSpan(w int) bool {
	switch w {
	case 3, 4, 6, 12:
		return true
	default:
		return false
	}
}

func newCategoryView(c storage.Category) categoryView {
	return categoryView{ID: c.ID, Name: c.Name, SortIndex: c.SortIndex, GridWidth: c.GridWidth}
}

// handleListCategories serves the shared catalog categories in sort_index order
// (SPEC-245-224). Session-gated like the rest of the catalog read; any logged-in
// user reads the same admin-managed set (#245 — no longer per-user). A kid
// reads only the categories holding at least one of its allowed services
// (AC-005, OQ-2): an empty section is hidden, and so is its name.
func (s *server) handleListCategories(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(w, r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	cats, err := s.store.ListCategories(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	visibleCats, ok := s.visibleCategoryIDs(w, r, u)
	if !ok {
		return
	}
	out := make([]categoryView, 0, len(cats))
	for _, c := range cats {
		if visibleCats == nil || visibleCats[c.ID] {
			out = append(out, newCategoryView(c))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": out})
}

// visibleCategoryIDs returns nil (every category) for an adult, and for a kid
// the ids of the categories that hold at least one of its allowed services. On
// error it has written a 500.
func (s *server) visibleCategoryIDs(w http.ResponseWriter, r *http.Request, u storage.User) (map[string]bool, bool) {
	vis, ok := s.visibleServices(w, r, u)
	if !ok {
		return nil, false
	}
	if vis == nil {
		return nil, true
	}
	svcs, ok := s.visibleCatalog(w, r, u)
	if !ok {
		return nil, false
	}
	ids := map[string]bool{}
	for _, sv := range svcs {
		if sv.CategoryID != nil {
			ids[*sv.CategoryID] = true
		}
	}
	return ids, true
}

// handleCreateCategory adds a category to the App Grid (issue #224, SPEC-app-grid
// §3A — "+Add box → POST /api/categories — Admin-only"). Creation is admin-gated:
// a non-admin session gets 403 (the frontend hides +Add, but the server must not
// trust the client gate). A duplicate name gets 409; the new category is appended
// last (sort_index max+1). This supersedes the v9/A4 per-user create model.
func (s *server) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
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

// handleUpdateCategory updates a section (unknown id → 404). Two independently
// optional fields: `name` (rename; a name collision → 409) and `gridWidth` (the
// App Grid box's 12-column span: 3, 4, 6 or 12 — SPEC-app-grid §10.4). A gridWidth-only PATCH must not
// require a name, and vice-versa; when both are present, rename then set width.
// At least one must be present. Admin-only (classCatalogWrite): a non-admin
// session gets 403.
func (s *server) handleUpdateCategory(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
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
	if in.GridWidth != nil && !validGridSpan(*in.GridWidth) {
		http.Error(w, "gridWidth must be one of 3, 4, 6, 12", http.StatusBadRequest)
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
		c, err = s.store.RenameCategory(r.Context(), id, name)
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
		c, err = s.store.SetCategoryWidth(r.Context(), id, *in.GridWidth)
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

// handleSetCategoryOrder reorders the sections whole-array, the same contract
// as PUT /api/layout. An id naming no section → 404. Success is 204.
// Admin-only (classCatalogWrite): a non-admin gets 403.
func (s *server) handleSetCategoryOrder(w http.ResponseWriter, r *http.Request) {
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

// handleDeleteCategory deletes a catalog section. Admin-only
// (classCatalogWrite): a non-admin gets 403. Idempotent: deleting an absent category is still 204. The FK is ON
// DELETE SET NULL, so the category's apps fall back to Uncategorized — none deleted.
func (s *server) handleDeleteCategory(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if err := s.store.DeleteCategory(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
