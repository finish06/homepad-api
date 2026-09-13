package api

import (
	"encoding/json"
	"net/http"
	"time"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// systemConfigView is the wire shape of the global System settings, shared by
// the public GET and the admin PATCH so both always echo the same fields.
type systemConfigView struct {
	ShowUptimeDisplay bool `json:"showUptimeDisplay"`
	// StatusDegradedMs is the EFFECTIVE "Slow" threshold: the admin-set value
	// when there is one, else what the poller is running with (env / default).
	StatusDegradedMs int `json:"statusDegradedMs"`
}

func (s *server) toSystemConfigView(cfg storage.SystemSettings) systemConfigView {
	ms := int(s.poller.DegradedAfter() / time.Millisecond)
	if cfg.StatusDegradedMs != nil {
		ms = *cfg.StatusDegradedMs
	}
	return systemConfigView{ShowUptimeDisplay: cfg.ShowUptimeDisplay, StatusDegradedMs: ms}
}

// maxStatusDegradedMs bounds the admin input (10 minutes); mirrors the DB CHECK.
const maxStatusDegradedMs = 600000

// handleSystemConfig serves the global System settings to anyone, no auth
// required (SPEC cap6 §7, D4) — the frontend reads it consistently regardless of
// session state. A missing row resolves to the default-ON config in the store
// layer (AC-008/010).
func (s *server) handleSystemConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.SystemSettings(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s.toSystemConfigView(cfg))
}

// handlePatchSystemSettings upserts the global System settings (admin only,
// SPEC cap6 §7, D5). The body is a partial patch — only present fields change —
// so it is merged over the current persisted values; the response echoes the
// full state after the write. requireAdmin writes 401 (no session) / 403
// (non-admin) itself (AC-011/012/013).
func (s *server) handlePatchSystemSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var in struct {
		ShowUptimeDisplay *bool `json:"showUptimeDisplay"`
		StatusDegradedMs  *int  `json:"statusDegradedMs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if in.StatusDegradedMs != nil && (*in.StatusDegradedMs < 0 || *in.StatusDegradedMs > maxStatusDegradedMs) {
		http.Error(w, "statusDegradedMs must be between 0 and 600000", http.StatusBadRequest)
		return
	}
	cur, err := s.store.SystemSettings(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if in.ShowUptimeDisplay != nil {
		cur.ShowUptimeDisplay = *in.ShowUptimeDisplay
	}
	if in.StatusDegradedMs != nil {
		v := *in.StatusDegradedMs
		cur.StatusDegradedMs = &v
	}
	saved, err := s.store.UpsertSystemSettings(r.Context(), cur)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Apply to the running poller at once — the next poll (scheduled or
	// "Retry now") uses it. No restart, which is the point of a UI setting.
	if saved.StatusDegradedMs != nil {
		s.poller.SetDegradedAfter(time.Duration(*saved.StatusDegradedMs) * time.Millisecond)
	}
	writeJSON(w, http.StatusOK, s.toSystemConfigView(saved))
}
