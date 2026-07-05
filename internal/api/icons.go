package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

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
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	variant := r.PathValue("variant")
	if !validVariant(variant) {
		http.Error(w, "variant must be light or dark", http.StatusBadRequest)
		return
	}

	ic, err := s.store.GetIcon(r.Context(), r.PathValue("id"), u.ID, variant)
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

// handlePutIcon stores (creates or replaces) a PNG variant on a shared catalog
// service. Admin-only under the shared catalog model (SPEC-245-224, #224): a
// non-admin session gets 403. The raw request body IS the PNG. Validation order:
// byte size (413) → PNG magic-byte sniff (415) → dimensions (422). 204 on success.
func (s *server) handlePutIcon(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
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
	err = s.store.PutIcon(r.Context(), r.PathValue("id"), u.ID, variant, body, cfg.Width, cfg.Height, etag)
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

// handleDeleteIcon removes a PNG variant on a shared catalog service. Admin-only
// under the shared catalog model (SPEC-245-224, #224): a non-admin session gets
// 403. Idempotent for the admin: 204 whether or not bytes existed (spec A11).
func (s *server) handleDeleteIcon(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	variant := r.PathValue("variant")
	if !validVariant(variant) {
		http.Error(w, "variant must be light or dark", http.StatusBadRequest)
		return
	}

	err := s.store.DeleteIcon(r.Context(), r.PathValue("id"), u.ID, variant)
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

// faviconTimeout bounds every fetch-icon HTTP hop (§7.4 step 2). A dedicated
// client (not http.DefaultClient) keeps the timeout scoped to this feature.
const faviconTimeout = 5 * time.Second

// linkTagRe / relRe / hrefRe sniff a favicon <link> out of HTML. This is a
// deliberate pragmatic match, NOT a full HTML parse — it avoids pulling in
// golang.org/x/net/html (a go.mod dependency, and the golangci go-directive
// trap). It finds any <link> tag whose rel names an icon and captures its href.
var (
	linkTagRe = regexp.MustCompile(`(?is)<link\b[^>]*>`)
	relRe     = regexp.MustCompile(`(?is)\brel\s*=\s*["']?([^"'>]+)`)
	hrefRe    = regexp.MustCompile(`(?is)\bhref\s*=\s*["']([^"']+)["']`)
)

// handleFetchIcon downloads the favicon from a service's own registered url and
// stores it as the light variant (v21 §7.4). Admin-only under the shared catalog
// model (SPEC-245-224): a non-admin gets 403. The pipeline is PNG-only, so a
// non-PNG favicon (or any fetch failure) returns 422 and leaves the icon as-is.
func (s *server) handleFetchIcon(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")

	rawURL, err := s.store.ServiceURL(r.Context(), id, u.ID)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "no such service", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	body, ferr := fetchFavicon(r.Context(), rawURL)
	if ferr != nil {
		http.Error(w, "Could not fetch favicon: "+ferr.Error(), http.StatusUnprocessableEntity)
		return
	}

	// Same PNG bounds as an upload (magic → dims), but every failure collapses to
	// 422 here — a fetched image that isn't a usable PNG is a fetch failure, not a
	// client error, and the existing icon must stay untouched (§7.4 step 8).
	if len(body) > iconMaxBytes {
		http.Error(w, "Could not fetch favicon: image exceeds 256 KB", http.StatusUnprocessableEntity)
		return
	}
	if len(body) < len(pngMagic) || !bytes.Equal(body[:len(pngMagic)], pngMagic) {
		http.Error(w, "Could not fetch favicon: not a PNG image", http.StatusUnprocessableEntity)
		return
	}
	cfg, derr := png.DecodeConfig(bytes.NewReader(body))
	if derr != nil || cfg.Width > iconMaxDim || cfg.Height > iconMaxDim ||
		cfg.Width < iconMinDim || cfg.Height < iconMinDim {
		http.Error(w, "Could not fetch favicon: unsupported image", http.StatusUnprocessableEntity)
		return
	}

	sum := sha256.Sum256(body)
	etag := hex.EncodeToString(sum[:])
	if err := s.store.PutIcon(r.Context(), id, u.ID, "light", body, cfg.Width, cfg.Height, etag); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.Error(w, "no such service", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"iconUrl": "/api/services/" + id + "/icon/light"})
}

// fetchFavicon resolves and downloads a service's favicon. It first parses the
// page HTML for a <link rel="icon"> (resolved against the page URL); failing
// that, it falls back to {origin}/favicon.ico (§7.4 steps 3–4). Returns the raw
// image bytes, or an error if nothing usable could be fetched.
func fetchFavicon(ctx context.Context, rawURL string) ([]byte, error) {
	base, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !base.IsAbs() || base.Host == "" {
		return nil, fmt.Errorf("invalid service url")
	}

	if href := discoverIconHref(ctx, base); href != "" {
		if ref, perr := base.Parse(href); perr == nil {
			if b, derr := downloadImage(ctx, ref.String()); derr == nil {
				return b, nil
			}
		}
	}

	fallback := url.URL{Scheme: base.Scheme, Host: base.Host, Path: "/favicon.ico"}
	if b, derr := downloadImage(ctx, fallback.String()); derr == nil {
		return b, nil
	}
	return nil, fmt.Errorf("no favicon found")
}

// discoverIconHref GETs the page and returns the href of the first <link> whose
// rel names an icon, or "" if the page can't be read or has no such link.
func discoverIconHref(ctx context.Context, page *url.URL) string {
	client := &http.Client{Timeout: faviconTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, page.String(), nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	// Cap the HTML we scan so a giant/streaming body can't exhaust memory.
	html, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return ""
	}
	for _, tag := range linkTagRe.FindAllString(string(html), -1) {
		rel := relRe.FindStringSubmatch(tag)
		if rel == nil || !strings.Contains(strings.ToLower(rel[1]), "icon") {
			continue
		}
		if href := hrefRe.FindStringSubmatch(tag); href != nil {
			return href[1]
		}
	}
	return ""
}

// downloadImage GETs an icon URL and returns its bytes. It rejects a non-200, a
// declared non-image content type (§7.4), and reads at most one byte past the
// icon cap so the handler's size check can fire.
func downloadImage(ctx context.Context, iconURL string) ([]byte, error) {
	client := &http.Client{Timeout: faviconTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, iconURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("icon fetch returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "image") {
		return nil, fmt.Errorf("icon content-type %q is not an image", ct)
	}
	return io.ReadAll(io.LimitReader(resp.Body, iconMaxBytes+1))
}
