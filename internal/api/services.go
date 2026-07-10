package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/gatus"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

type serviceView struct {
	ID           string  `json:"id"`
	Slug         string  `json:"slug"`
	Name         string  `json:"name"`
	Description  string  `json:"description"`
	URL          string  `json:"url"`
	Icon         string  `json:"icon"`
	Status       string  `json:"status"`
	Favorite     bool    `json:"favorite"`
	IconLight    bool    `json:"iconLight"`
	IconDark     bool    `json:"iconDark"`
	CategoryID   *string `json:"categoryId"`   // null when Uncategorized (v4)
	CategoryName *string `json:"categoryName"` // null when Uncategorized; denormalized
	// SourceLibraryID is provenance only (v9, C1): the library offer a copy was
	// added from, null for a custom app. Additive — never changes behavior.
	SourceLibraryID *string `json:"sourceLibraryId"`
	// ClickAction (v23) is the tile's click behavior — 'new_tab' | 'same_tab' |
	// 'iframe'. Always present on the wire (DB default 'new_tab'); a pre-migration
	// client that omits it is treated as new_tab by the frontend.
	ClickAction string `json:"clickAction"`
	// UptimeChecks is the recent Gatus history (≤20, oldest-first) backing the
	// tile sparkline. Always present; [] when the service has no gatus_key or no
	// cached results. Additive — clients ignoring it are unaffected.
	UptimeChecks []checkResultView `json:"uptimeChecks"`
	// UptimeWindows is Gatus's own computed availability per long window
	// ("24h"/"7d"/"30d"), fraction 0..1. Always present; {} when unmonitored or
	// no data. A window Gatus couldn't answer is omitted. Additive.
	UptimeWindows map[string]float64 `json:"uptimeWindows"`
}

// validClickAction reports whether v is one of the three v23 click-action enum
// values. Empty is a valid input on create (the storage layer applies the DB
// default 'new_tab'); callers that must reject empty check that separately.
func validClickAction(v string) bool {
	switch v {
	case "", "new_tab", "same_tab", "iframe":
		return true
	default:
		return false
	}
}

// checkResultView is the wire form of one historical Gatus check.
type checkResultView struct {
	Success   bool      `json:"success"`
	Timestamp time.Time `json:"timestamp"`
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

	owner, err := s.store.SharedCatalogOwnerID(r.Context())
	if errors.Is(err, storage.ErrNotFound) {
		// No admin → no shared catalog yet; serve an empty list rather than 500.
		writeJSON(w, http.StatusOK, map[string]any{"services": []serviceView{}})
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Shared set (owner's rows) decorated with the caller's own layout order.
	svcs, err := s.store.ListServices(r.Context(), owner, u.ID)
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
			ID:              sv.ID,
			Slug:            sv.Slug,
			Name:            sv.Name,
			Description:     sv.Description,
			URL:             sv.URL,
			Icon:            sv.Icon,
			Status:          statusFor(snap, sv.GatusKey),
			Favorite:        favs[sv.ID],
			IconLight:       icons[sv.ID].Light,
			IconDark:        icons[sv.ID].Dark,
			CategoryID:      sv.CategoryID,
			CategoryName:    sv.CategoryName,
			SourceLibraryID: sv.SourceLibraryID,
			ClickAction:     sv.ClickAction,
			UptimeChecks:    uptimeChecksFor(snap, sv.GatusKey),
			UptimeWindows:   uptimeWindowsFor(snap, sv.GatusKey),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": out})
}

// handleCreateService adds a service to the shared catalog. Admin-only under the
// shared catalog model (SPEC-245-224, #224): a non-admin session gets 403. The
// row is owned by the acting admin (the shared-catalog owner).
func (s *server) handleCreateService(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}

	var in struct {
		Slug        string `json:"slug"`
		Name        string `json:"name"`
		Description string `json:"description"`
		URL         string `json:"url"`
		Icon        string `json:"icon"`
		GatusKey    string `json:"gatus_key"`
		ClickAction string `json:"clickAction"`
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
	// v23 — an omitted clickAction defaults to new_tab in storage; a present but
	// unknown value is a 400. (§3.2)
	if !validClickAction(in.ClickAction) {
		http.Error(w, "clickAction must be one of new_tab, same_tab, iframe", http.StatusBadRequest)
		return
	}

	sv, err := s.store.CreateService(r.Context(), u.ID, storage.Service{
		Slug:        in.Slug,
		Name:        in.Name,
		Description: in.Description,
		URL:         in.URL,
		Icon:        in.Icon,
		GatusKey:    strings.TrimSpace(in.GatusKey),
		ClickAction: in.ClickAction,
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
		ClickAction: sv.ClickAction,
	})
}

// handleUpdateService edits a shared catalog service. Admin-only under the
// shared catalog model (SPEC-245-224, #224): a non-admin session gets 403. Body
// fields are optional — only those present are changed.
func (s *server) handleUpdateService(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}

	var in struct {
		Slug        *string        `json:"slug"`
		Name        *string        `json:"name"`
		Description *string        `json:"description"`
		URL         *string        `json:"url"`
		Icon        *string        `json:"icon"`
		GatusKey    *string        `json:"gatus_key"`
		CategoryID  optionalString `json:"categoryId"`
		ClickAction *string        `json:"clickAction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	// v23 — when clickAction is present it must be a valid enum value (an explicit
	// empty string is not a valid change; omit the key to leave it unchanged). §3.2
	if in.ClickAction != nil && (*in.ClickAction == "" || !validClickAction(*in.ClickAction)) {
		http.Error(w, "clickAction must be one of new_tab, same_tab, iframe", http.StatusBadRequest)
		return
	}

	sv, err := s.store.UpdateService(r.Context(), r.PathValue("id"), u.ID, storage.ServiceUpdate{
		Slug:        in.Slug,
		Name:        in.Name,
		Description: in.Description,
		URL:         in.URL,
		Icon:        in.Icon,
		GatusKey:    in.GatusKey,
		SetCategory: in.CategoryID.Set,
		CategoryID:  in.CategoryID.Value,
		ClickAction: in.ClickAction,
	})
	if errors.Is(err, storage.ErrCategoryNotFound) {
		http.Error(w, "no such category", http.StatusBadRequest)
		return
	}
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
		ClickAction:     sv.ClickAction,
	})
}

// optionalString distinguishes JSON's three states for a nullable field: the
// key absent (Set false → leave unchanged), present as null (Set true, Value
// nil → clear), or present with a string (Set true, Value set). UnmarshalJSON
// only fires when the key is present, so absence is captured for free.
type optionalString struct {
	Set   bool
	Value *string
}

func (o *optionalString) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// handleDeleteService removes a shared catalog service. Admin-only under the
// shared catalog model (SPEC-245-224, #224): a non-admin session gets 403.
func (s *server) handleDeleteService(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}

	err := s.store.DeleteService(r.Context(), r.PathValue("id"), u.ID)
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

// statusFor resolves a service's badge from the snapshot. An empty gatus_key
// means monitoring was never wired → NOT_MONITORED. A key that IS set but has no
// cached result (e.g. Gatus unreachable) is a monitoring failure → UNKNOWN.
func statusFor(snap gatus.Snapshot, gatusKey string) string {
	if gatusKey == "" {
		return gatus.StatusNotMonitored
	}
	if st, ok := snap.Statuses[gatusKey]; ok {
		return st.Status
	}
	return gatus.StatusUnknown
}

// uptimeChecksFor maps a service's cached Gatus history to the wire view. It
// always returns a non-nil slice so the JSON field is [] (never null): no
// gatus_key, or no cached results for it (e.g. Gatus unreachable), yields [].
func uptimeChecksFor(snap gatus.Snapshot, gatusKey string) []checkResultView {
	out := []checkResultView{}
	if gatusKey == "" {
		return out
	}
	st, ok := snap.Statuses[gatusKey]
	if !ok {
		return out
	}
	for _, r := range st.Results {
		out = append(out, checkResultView{Success: r.Success, Timestamp: r.Timestamp})
	}
	return out
}

// uptimeWindowsFor surfaces Gatus's computed long-window uptime for a service's
// gatus_key. Always returns a non-nil map so the JSON is {} (never null): empty
// for an unmonitored service or one with no snapshot/uptime data.
func uptimeWindowsFor(snap gatus.Snapshot, gatusKey string) map[string]float64 {
	out := map[string]float64{}
	if gatusKey == "" {
		return out
	}
	st, ok := snap.Statuses[gatusKey]
	if !ok {
		return out
	}
	for win, v := range st.Uptime {
		out[win] = v
	}
	return out
}
