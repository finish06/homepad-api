package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// v21 §7.4 — POST /api/services/{id}/fetch-icon fetches the favicon from the
// service's own registered url and stores it as the light icon variant. It is
// admin-only (the same shared-catalog write gate as icon upload) and returns a
// clean 422 on any failure, leaving the existing icon untouched. The pipeline is
// PNG-only (the icon store validates + serves PNG); a non-PNG favicon 422s.

// createServiceForFetch POSTs a fresh shared-catalog service pointing at `url`
// and returns its id, so a test can drive fetch-icon against a controlled site.
func createServiceForFetch(t *testing.T, baseURL, token, url string) string {
	t.Helper()
	r := doJSON(t, http.MethodPost, baseURL+"/api/services", token, map[string]any{
		"slug": "fetch-target", "name": "Fetch Target", "url": url,
	})
	defer r.Body.Close()
	require.Equal(t, http.StatusCreated, r.StatusCode)
	var sv struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(r.Body).Decode(&sv))
	require.NotEmpty(t, sv.ID)
	return sv.ID
}

// TestFetchIcon_StoresFaviconFromLinkTag — the happy path: the site advertises a
// PNG favicon via <link rel="icon" href="/brand.png">; fetch-icon downloads it
// and stores it as the light variant, which then serves those exact bytes.
func TestFetchIcon_StoresFaviconFromLinkTag(t *testing.T) {
	png := pngBytes(t, 32, 32)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<html><head><link rel="icon" href="/brand.png"></head></html>`)
		case "/brand.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png)
		default:
			http.NotFound(w, r)
		}
	}))
	defer target.Close()

	s := testsupport.NewServer(t)
	defer s.Close()
	id := createServiceForFetch(t, s.URL, "admin-session", target.URL)

	r := doJSON(t, http.MethodPost, s.URL+"/api/services/"+id+"/fetch-icon", "admin-session", nil)
	defer r.Body.Close()
	require.Equal(t, http.StatusOK, r.StatusCode)
	var body struct {
		IconURL string `json:"iconUrl"`
	}
	require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
	assert.Equal(t, "/api/services/"+id+"/icon/light", body.IconURL)

	got := doJSON(t, http.MethodGet, s.URL+"/api/services/"+id+"/icon/light", "admin-session", nil)
	defer got.Body.Close()
	require.Equal(t, http.StatusOK, got.StatusCode)
	gotBytes, _ := io.ReadAll(got.Body)
	assert.Equal(t, png, gotBytes, "the stored light icon serves the fetched favicon bytes")
}

// TestFetchIcon_FallsBackToFaviconIco — with no <link> in the HTML, the server
// falls back to {origin}/favicon.ico (§7.4 step 4). Here that path serves PNG
// bytes, so the fetch succeeds.
func TestFetchIcon_FallsBackToFaviconIco(t *testing.T) {
	png := pngBytes(t, 32, 32)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<html><head><title>no icon link</title></head></html>`)
		case "/favicon.ico":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png)
		default:
			http.NotFound(w, r)
		}
	}))
	defer target.Close()

	s := testsupport.NewServer(t)
	defer s.Close()
	id := createServiceForFetch(t, s.URL, "admin-session", target.URL)

	r := doJSON(t, http.MethodPost, s.URL+"/api/services/"+id+"/fetch-icon", "admin-session", nil)
	defer r.Body.Close()
	assert.Equal(t, http.StatusOK, r.StatusCode)
}

// TestFetchIcon_NoFaviconFound_422 — no link tag and no favicon.ico: fetch-icon
// returns 422 and leaves the service with no stored icon (GET → 404).
func TestFetchIcon_NoFaviconFound_422(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<html><head></head><body>no favicon anywhere</body></html>`)
			return
		}
		http.NotFound(w, r)
	}))
	defer target.Close()

	s := testsupport.NewServer(t)
	defer s.Close()
	id := createServiceForFetch(t, s.URL, "admin-session", target.URL)

	r := doJSON(t, http.MethodPost, s.URL+"/api/services/"+id+"/fetch-icon", "admin-session", nil)
	defer r.Body.Close()
	assert.Equal(t, http.StatusUnprocessableEntity, r.StatusCode)

	got := doJSON(t, http.MethodGet, s.URL+"/api/services/"+id+"/icon/light", "admin-session", nil)
	defer got.Body.Close()
	assert.Equal(t, http.StatusNotFound, got.StatusCode, "a failed fetch leaves the icon unchanged")
}

// TestFetchIcon_NonAdmin_403 — fetch-icon is a catalog write, so a non-admin is
// forbidden (AC-016 family / SPEC-245-224).
func TestFetchIcon_NonAdmin_403(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	id := listServices(t, s.URL, "admin-session")[0].ID

	r := doJSON(t, http.MethodPost, s.URL+"/api/services/"+id+"/fetch-icon", "non-admin-session", nil)
	defer r.Body.Close()
	assert.Equal(t, http.StatusForbidden, r.StatusCode)
}
