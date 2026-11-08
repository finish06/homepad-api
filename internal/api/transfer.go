package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/transfer"
)

// SPEC-backup-import §8 — export, restore and Homepage import. All five
// routes are admin-only. Every write goes to the shared catalog owner, never
// the acting admin's own id (AC-025, homepad-api#87). Errors are JSON
// ({code, message, …}) so the UI owns the words (UX §6); 401/403 stay the
// shared plain-text answers from requireAdmin.

// Version is the Homepad version written into a backup's homepadVersion. It
// is set at build time (-ldflags "-X …/internal/api.Version=17.1.0"); nothing
// reads it back (§2.3).
var Version = "dev"

const (
	// maxRestoreBody caps a backup upload (AC-026).
	maxRestoreBody = 16 << 20
	// maxImportBody caps the Homepage import's JSON body: three files of up
	// to 1 MB each, with room for JSON escaping. Each file is then checked
	// against its own 1 MB limit.
	maxImportBody = 8 << 20
)

// errorBody is the JSON error for these routes. Plan is set on a 409.
type errorBody struct {
	*transfer.Error
	Plan *transfer.Plan `json:"plan,omitempty"`
}

func writeTransferError(w http.ResponseWriter, e *transfer.Error) {
	writeJSON(w, e.Status, errorBody{Error: e})
}

func badRequest(code, msg string) *transfer.Error {
	return &transfer.Error{Status: http.StatusBadRequest, Code: code, Message: msg}
}

// readBody reads at most limit bytes; more is a 413. A declared length over
// the limit is refused before reading anything (http.MaxBytesReader catches
// a body that lies about its length, or has none).
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, *transfer.Error) {
	tooLarge := &transfer.Error{Status: http.StatusRequestEntityTooLarge, Code: transfer.CodeTooLarge,
		Message: "This file is too big. Backups can be up to 16 MB."}
	if r.ContentLength > limit {
		return nil, tooLarge
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return nil, tooLarge
	}
	if err != nil {
		return nil, badRequest(transfer.CodeInvalidJSON, "The upload didn't finish.")
	}
	return b, nil
}

func restoreMode(r *http.Request) (string, *transfer.Error) {
	switch m := r.URL.Query().Get("mode"); m {
	case "", transfer.ModeMerge:
		return transfer.ModeMerge, nil
	case transfer.ModeReplace:
		return transfer.ModeReplace, nil
	default:
		return "", badRequest(transfer.CodeBadMode, "mode must be merge or replace")
	}
}

func planParam(r *http.Request) (string, *transfer.Error) {
	h := r.URL.Query().Get("plan")
	if h == "" {
		return "", badRequest(transfer.CodePlanMissing, "plan is required: preview first")
	}
	return h, nil
}

// handleExport — GET /api/admin/export?people=true|false (§2.1, AC-001…005).
func (s *server) handleExport(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	people := true
	switch r.URL.Query().Get("people") {
	case "", "true":
	case "false":
		people = false
	default:
		writeTransferError(w, badRequest("bad_people", "people must be true or false"))
		return
	}
	owner, ok := s.catalogOwner(w, r, "no catalog yet")
	if !ok {
		return
	}
	st, err := s.store.LoadTransferState(r.Context(), storage.TransferLoad{OwnerID: owner, CallerID: u.ID, People: people, IconBytes: true})
	if err != nil {
		log.Printf("transfer: export: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	body := transfer.Encode(transfer.BuildFile(st, people, Version, now))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="homepad-backup-%s.json"`, now.Format("2006-01-02")))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleRestorePreview — POST /api/admin/restore/preview?mode= (§2.2). It
// reads only: the database is untouched (AC-007).
func (s *server) handleRestorePreview(w http.ResponseWriter, r *http.Request) {
	s.restore(w, r, false)
}

// handleRestore — POST /api/admin/restore?mode=&plan= (§2.2, AC-008, AC-009).
func (s *server) handleRestore(w http.ResponseWriter, r *http.Request) {
	s.restore(w, r, true)
}

func (s *server) restore(w http.ResponseWriter, r *http.Request, apply bool) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	mode, terr := restoreMode(r)
	if terr != nil {
		writeTransferError(w, terr)
		return
	}
	want := ""
	if apply {
		if want, terr = planParam(r); terr != nil {
			writeTransferError(w, terr)
			return
		}
	}
	body, terr := readBody(w, r, maxRestoreBody)
	if terr != nil {
		writeTransferError(w, terr)
		return
	}
	d, terr := transfer.DecodeBackup(body)
	if terr != nil {
		writeTransferError(w, terr)
		return
	}
	plan := func(st *storage.TransferState) *transfer.Plan { return transfer.PlanRestore(st, d, mode) }
	s.previewOrApply(w, r, u, "restore", mode, storage.TransferLoad{People: d.File.People != nil}, plan, apply, want)
}

// handleImportPreview — POST /api/admin/import/homepage/preview (§2.4).
func (s *server) handleImportPreview(w http.ResponseWriter, r *http.Request) {
	s.importHomepage(w, r, false)
}

// handleImport — POST /api/admin/import/homepage?plan= (§2.4).
func (s *server) handleImport(w http.ResponseWriter, r *http.Request) {
	s.importHomepage(w, r, true)
}

func (s *server) importHomepage(w http.ResponseWriter, r *http.Request, apply bool) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	want := ""
	var terr *transfer.Error
	if apply {
		if want, terr = planParam(r); terr != nil {
			writeTransferError(w, terr)
			return
		}
	}
	body, terr := readBody(w, r, maxImportBody)
	if terr != nil {
		terr.Message = "This file is too big. Homepage files can be up to 1 MB."
		writeTransferError(w, terr)
		return
	}
	var in transfer.HomepageInput
	if err := json.Unmarshal(body, &in); err != nil {
		writeTransferError(w, badRequest(transfer.CodeInvalidJSON, "The request body must be {services, bookmarks, settings} as text."))
		return
	}
	hp, terr := transfer.ParseHomepage(in)
	if terr != nil {
		writeTransferError(w, terr)
		return
	}
	plan := func(st *storage.TransferState) *transfer.Plan {
		return transfer.PlanImport(st, hp, s.gatusKeys())
	}
	s.previewOrApply(w, r, u, "import", transfer.ModeImport, storage.TransferLoad{}, plan, apply, want)
}

// gatusKeys is the set of keys in the poller's in-memory snapshot. No
// outbound call (§9).
func (s *server) gatusKeys() map[string]bool {
	snap := s.poller.Snapshot()
	keys := make(map[string]bool, len(snap.Statuses))
	for k := range snap.Statuses {
		keys[k] = true
	}
	return keys
}

// errPlanChanged carries the fresh plan out of the transaction for the 409.
type errPlanChanged struct{ plan *transfer.Plan }

func (errPlanChanged) Error() string { return "transfer: plan changed" }

// previewOrApply is the shared second half of restore and import. A preview
// plans from a read-only snapshot. An apply re-plans inside one transaction
// holding the import lock, refuses with 409 (and the new plan) when the hash
// moved, writes everything or nothing, and logs one line (AC-030).
func (s *server) previewOrApply(w http.ResponseWriter, r *http.Request, u storage.User, what, mode string,
	load storage.TransferLoad, plan func(*storage.TransferState) *transfer.Plan, apply bool, want string) {
	owner, ok := s.catalogOwner(w, r, "no catalog yet")
	if !ok {
		return
	}
	load.OwnerID, load.CallerID, load.AccountLimit = owner, u.ID, s.maxAccounts
	if !apply {
		st, err := s.store.LoadTransferState(r.Context(), load)
		if err != nil {
			log.Printf("transfer: %s preview: %v", what, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, plan(st))
		return
	}

	var applied *transfer.Plan
	err := s.store.InTransferTx(r.Context(), s.maxAccounts, func(tx *storage.TransferTx) error {
		st, err := tx.LoadState(r.Context(), load)
		if err != nil {
			return err
		}
		p := plan(st)
		if p.PlanHash != want {
			return errPlanChanged{plan: p}
		}
		var wr transfer.Writer = tx
		if s.transferWrap != nil {
			wr = s.transferWrap(wr)
		}
		if err := transfer.Apply(r.Context(), wr, p); err != nil {
			return err
		}
		applied = p
		return nil
	})
	var changed errPlanChanged
	if errors.As(err, &changed) {
		writeJSON(w, http.StatusConflict, errorBody{
			Error: &transfer.Error{Status: http.StatusConflict, Code: transfer.CodePlanChanged,
				Message: "Your dashboard changed since the preview. Here's the new one."},
			Plan: changed.plan,
		})
		return
	}
	if errors.Is(err, storage.ErrAccountLimit) {
		// The plan leaves out kids past the cap, so only a cap lowered under
		// a running apply lands here.
		writeTransferError(w, &transfer.Error{Status: http.StatusConflict, Code: transfer.ReasonAccountLimit,
			Message: accountLimitMessage(s.maxAccounts)})
		return
	}
	if err != nil {
		log.Printf("transfer: %s failed, nothing changed: %v", what, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sum := applied.Summary
	log.Printf("transfer: %s mode=%s by=%s created sections=%d apps=%d icons=%d offers=%d kids=%d updated sections=%d apps=%d icons=%d offers=%d people=%d settings=%d deleted sections=%d apps=%d offers=%d unchanged=%d skipped=%d",
		what, mode, u.Email,
		sum.Create.Sections, sum.Create.Apps, sum.Create.Icons, sum.Create.Offers, sum.Create.Kids,
		sum.Update.Sections, sum.Update.Apps, sum.Update.Icons, sum.Update.Offers, sum.Update.People, sum.Update.Settings,
		sum.Delete.Sections, sum.Delete.Apps, sum.Delete.Offers, sum.Unchanged, sum.Skipped)
	// A restored "Slow" threshold applies to the next poll, as a System
	// panel change does.
	if what == "restore" {
		if cfg, err := s.store.SystemSettings(r.Context()); err == nil && cfg.StatusDegradedMs != nil {
			s.poller.SetDegradedAfter(time.Duration(*cfg.StatusDegradedMs) * time.Millisecond)
		}
	}
	writeJSON(w, http.StatusOK, applied.Result())
}
