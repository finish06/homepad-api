package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image/png"
	"io"
	"net/http"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// Icon validation caps (spec v2-app-icons). Kept in one place so they're
// trivial to retune; reject (not downscale) is the v2 policy.
const (
	iconMaxBytes = 256 * 1024 // 256 KB per variant
	iconMaxDim   = 512        // px
	iconMinDim   = 16         // px — rejects 1-px tracking junk
)

// pngMagic is the 8-byte PNG signature we sniff to validate format, ignoring
// the client's Content-Type and filename (spec A5).
var pngMagic = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// validVariant guards the {variant} path segment.
func validVariant(v string) bool { return v == "light" || v == "dark" }

// handleGetIcon serves a service's uploaded PNG variant. Session-gated like the
// rest of /api/* — the <img> carries the same-site session cookie. 404 when the
// variant has no upload; honors If-None-Match → 304 (spec A10).
func (s *server) handleGetIcon(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentUser(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	variant := r.PathValue("variant")
	if !validVariant(variant) {
		http.Error(w, "variant must be light or dark", http.StatusBadRequest)
		return
	}

	ic, err := s.store.GetIcon(r.Context(), r.PathValue("id"), variant)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no icon for that variant", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	etag := `"` + ic.ETag + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=300")
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(ic.Bytes)
}

// handlePutIcon stores (creates or replaces) a service's PNG variant. Admin-only
// (403 otherwise). The raw request body IS the PNG. Validation order: byte size
// (413) → PNG magic-byte sniff (415) → dimensions (422). 204 on success.
func (s *server) handlePutIcon(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if u.Role != "admin" {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	variant := r.PathValue("variant")
	if !validVariant(variant) {
		http.Error(w, "variant must be light or dark", http.StatusBadRequest)
		return
	}

	// Read one byte past the cap so we can tell "exactly at the limit" from "over".
	body, err := io.ReadAll(io.LimitReader(r.Body, iconMaxBytes+1))
	if err != nil {
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}
	if len(body) > iconMaxBytes {
		http.Error(w, "icon exceeds 256 KB", http.StatusRequestEntityTooLarge)
		return
	}
	if len(body) < len(pngMagic) || !bytes.Equal(body[:len(pngMagic)], pngMagic) {
		http.Error(w, "body is not a PNG", http.StatusUnsupportedMediaType)
		return
	}

	cfg, err := png.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		http.Error(w, "corrupt PNG", http.StatusUnprocessableEntity)
		return
	}
	if cfg.Width > iconMaxDim || cfg.Height > iconMaxDim ||
		cfg.Width < iconMinDim || cfg.Height < iconMinDim {
		http.Error(w, "icon must be between 16x16 and 512x512", http.StatusUnprocessableEntity)
		return
	}

	sum := sha256.Sum256(body)
	etag := hex.EncodeToString(sum[:])
	err = s.store.PutIcon(r.Context(), r.PathValue("id"), variant, body, cfg.Width, cfg.Height, etag)
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

// handleDeleteIcon removes a service's PNG variant. Admin-only (403 otherwise).
// Idempotent: 204 whether or not bytes existed (spec A11).
func (s *server) handleDeleteIcon(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if u.Role != "admin" {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	variant := r.PathValue("variant")
	if !validVariant(variant) {
		http.Error(w, "variant must be light or dark", http.StatusBadRequest)
		return
	}

	if err := s.store.DeleteIcon(r.Context(), r.PathValue("id"), variant); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
