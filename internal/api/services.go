package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/gatus"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

type serviceView struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Icon        string `json:"icon"`
	Status      string `json:"status"`
	Favorite    bool   `json:"favorite"`
	IconLight   bool   `json:"iconLight"`
	IconDark    bool   `json:"iconDark"`
}

// handleListServices serves the shared catalog with a live status badge per
// tile, merged from the in-memory Gatus snapshot. It never proxies to Gatus, so
// it cannot 5xx on Gatus being down (A9) — those tiles just resolve to UNKNOWN.
func (s *server) handleListServices(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	svcs, err := s.store.ListServices(r.Context(), u.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	favs, err := s.store.FavoriteIDs(r.Context(), u.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	icons, err := s.store.AllIconFlags(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	snap := s.poller.Snapshot()
	out := make([]serviceView, 0, len(svcs))
	for _, sv := range svcs {
		out = append(out, serviceView{
			ID:          sv.ID,
			Slug:        sv.Slug,
			Name:        sv.Name,
			Description: sv.Description,
			URL:         sv.URL,
			Icon:        sv.Icon,
			Status:      statusFor(snap, sv.GatusKey),
			Favorite:    favs[sv.ID],
			IconLight:   icons[sv.ID].Light,
			IconDark:    icons[sv.ID].Dark,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": out})
}

// handleCreateService lets an admin add a catalog entry (A6). Non-admins get 403.
func (s *server) handleCreateService(w http.ResponseWriter, r *http.Request) {
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
		Slug        string `json:"slug"`
		Name        string `json:"name"`
		Description string `json:"description"`
		URL         string `json:"url"`
		Icon        string `json:"icon"`
		GatusKey    string `json:"gatus_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	in.Slug = strings.TrimSpace(in.Slug)
	in.Name = strings.TrimSpace(in.Name)
	in.URL = strings.TrimSpace(in.URL)
	if in.Slug == "" || in.Name == "" || in.URL == "" {
		http.Error(w, "slug, name and url are required", http.StatusBadRequest)
		return
	}

	sv, err := s.store.CreateService(r.Context(), storage.Service{
		Slug:        in.Slug,
		Name:        in.Name,
		Description: in.Description,
		URL:         in.URL,
		Icon:        in.Icon,
		GatusKey:    strings.TrimSpace(in.GatusKey),
	})
	if errors.Is(err, storage.ErrSlugTaken) {
		http.Error(w, "a service with that slug already exists", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, serviceView{
		ID:          sv.ID,
		Slug:        sv.Slug,
		Name:        sv.Name,
		Description: sv.Description,
		URL:         sv.URL,
		Icon:        sv.Icon,
		Status:      statusFor(s.poller.Snapshot(), sv.GatusKey),
	})
}

// handleUpdateService lets an admin edit a catalog entry (A6). Non-admins get
// 403. Body fields are optional — only those present are changed.
func (s *server) handleUpdateService(w http.ResponseWriter, r *http.Request) {
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
		Slug        *string `json:"slug"`
		Name        *string `json:"name"`
		Description *string `json:"description"`
		URL         *string `json:"url"`
		Icon        *string `json:"icon"`
		GatusKey    *string `json:"gatus_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	sv, err := s.store.UpdateService(r.Context(), r.PathValue("id"), storage.ServiceUpdate{
		Slug:        in.Slug,
		Name:        in.Name,
		Description: in.Description,
		URL:         in.URL,
		Icon:        in.Icon,
		GatusKey:    in.GatusKey,
	})
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such service", http.StatusNotFound)
		return
	}
	if errors.Is(err, storage.ErrSlugTaken) {
		http.Error(w, "a service with that slug already exists", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, serviceView{
		ID:          sv.ID,
		Slug:        sv.Slug,
		Name:        sv.Name,
		Description: sv.Description,
		URL:         sv.URL,
		Icon:        sv.Icon,
		Status:      statusFor(s.poller.Snapshot(), sv.GatusKey),
	})
}

// handleDeleteService lets an admin remove a catalog entry (A6). Non-admins get 403.
func (s *server) handleDeleteService(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if u.Role != "admin" {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}

	err := s.store.DeleteService(r.Context(), r.PathValue("id"))
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

// handleStatus exposes the raw cached Gatus snapshot for the frontend's
// staleness display (A4). Keys are Gatus endpoint keys, never the Gatus URL (A11).
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentUser(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	snap := s.poller.Snapshot()
	statuses := make(map[string]string, len(snap.Statuses))
	for key, st := range snap.Statuses {
		statuses[key] = st.Status
	}
	writeJSON(w, http.StatusOK, map[string]any{"as_of": snap.AsOf, "statuses": statuses})
}

// statusFor resolves a service's badge from the snapshot. No gatus_key, or no
// cached result for it (e.g. Gatus unreachable), resolves to UNKNOWN.
func statusFor(snap gatus.Snapshot, gatusKey string) string {
	if gatusKey == "" {
		return gatus.StatusUnknown
	}
	if st, ok := snap.Statuses[gatusKey]; ok {
		return st.Status
	}
	return gatus.StatusUnknown
}
