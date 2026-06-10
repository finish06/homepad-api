package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// AC A3, A4, A5, A6, A10, A11, A12, A13 — custom app icons (v2).

// pngBytes encodes a w×h PNG. A blank image compresses to a few hundred bytes,
// so it stays well under the 256 KB cap regardless of dimensions.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// noisyPNGOver256K returns a valid 512×512 PNG whose high-entropy pixels defeat
// compression, pushing the encoded size past the 256 KB cap for the A6 case.
func noisyPNGOver256K(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 512, 512))
	// Deterministic LCG fill — incompressible without relying on math/rand.
	var seed uint32 = 0x12345678
	for i := range img.Pix {
		seed = seed*1664525 + 1013904223
		img.Pix[i] = byte(seed >> 24)
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	require.Greater(t, buf.Len(), 256*1024, "fixture must exceed 256 KB to exercise 413")
	return buf.Bytes()
}

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	img.Set(0, 0, color.White)
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, nil))
	return buf.Bytes()
}

func firstServiceID(t *testing.T, baseURL, token string) string {
	t.Helper()
	svcs := listServices(t, baseURL, token)
	require.NotEmpty(t, svcs, "expected seeded services in the catalog")
	return svcs[0].ID
}

func putIcon(t *testing.T, baseURL, token, id, variant string, body []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, baseURL+"/api/services/"+id+"/icon/"+variant, bytes.NewReader(body))
	req.Header.Set("Content-Type", "image/png")
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: token})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// A3 — admin PUTs light+dark; GET returns each with image/png and the exact bytes.
func TestAdminUploadAndServeIcons(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	id := firstServiceID(t, s.URL, "admin-session")

	light := pngBytes(t, 48, 48)
	dark := pngBytes(t, 64, 64)
	for _, tc := range []struct {
		variant string
		want    []byte
	}{{"light", light}, {"dark", dark}} {
		resp := putIcon(t, s.URL, "admin-session", id, tc.variant, tc.want)
		require.Equal(t, http.StatusNoContent, resp.StatusCode, "PUT %s must be 204", tc.variant)
		resp.Body.Close()

		got, _ := http.NewRequest(http.MethodGet, s.URL+"/api/services/"+id+"/icon/"+tc.variant, nil)
		got.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
		gr, err := http.DefaultClient.Do(got)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, gr.StatusCode)
		assert.Equal(t, "image/png", gr.Header.Get("Content-Type"))
		b, _ := io.ReadAll(gr.Body)
		gr.Body.Close()
		assert.True(t, bytes.Equal(tc.want, b), "GET %s must return the uploaded bytes", tc.variant)
	}
}

// A4 — non-admin gets 403 on PUT and DELETE of any variant.
func TestNonAdminCannotMutateIcons(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	id := firstServiceID(t, s.URL, "non-admin-session")

	resp := putIcon(t, s.URL, "non-admin-session", id, "light", pngBytes(t, 32, 32))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "non-admin PUT must be 403")
	resp.Body.Close()

	del, _ := http.NewRequest(http.MethodDelete, s.URL+"/api/services/"+id+"/icon/light", nil)
	del.AddCookie(&http.Cookie{Name: "homepad_session", Value: "non-admin-session"})
	dr, err := http.DefaultClient.Do(del)
	require.NoError(t, err)
	defer dr.Body.Close()
	assert.Equal(t, http.StatusForbidden, dr.StatusCode, "non-admin DELETE must be 403")
}

// A5 — JPEG bytes labeled image/png are rejected by magic-byte sniff with 415,
// and no row is written (a follow-up GET is 404).
func TestSpoofedContentTypeRejected415(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	id := firstServiceID(t, s.URL, "admin-session")

	resp := putIcon(t, s.URL, "admin-session", id, "light", jpegBytes(t))
	assert.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode,
		"JPEG labeled image/png must be 415 by magic-byte sniff")
	resp.Body.Close()

	got, _ := http.NewRequest(http.MethodGet, s.URL+"/api/services/"+id+"/icon/light", nil)
	got.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
	gr, err := http.DefaultClient.Do(got)
	require.NoError(t, err)
	defer gr.Body.Close()
	assert.Equal(t, http.StatusNotFound, gr.StatusCode, "no row must be written on a rejected upload")
}

// A6 — boundary validation: >512px → 422, >256 KB → 413, valid → 204.
func TestIconSizeAndDimensionLimits(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	id := firstServiceID(t, s.URL, "admin-session")

	cases := []struct {
		name string
		body []byte
		want int
	}{
		{"over-dimensions", pngBytes(t, 513, 513), http.StatusUnprocessableEntity},
		{"under-dimensions", pngBytes(t, 8, 8), http.StatusUnprocessableEntity},
		{"over-bytes", noisyPNGOver256K(t), http.StatusRequestEntityTooLarge},
		{"valid", pngBytes(t, 256, 256), http.StatusNoContent},
		{"max-dimensions-ok", pngBytes(t, 512, 512), http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := putIcon(t, s.URL, "admin-session", id, "light", tc.body)
			defer resp.Body.Close()
			assert.Equal(t, tc.want, resp.StatusCode)
		})
	}
}

// A6 (cont.) — a bad variant in the path is a 400.
func TestBadVariantRejected400(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	id := firstServiceID(t, s.URL, "admin-session")

	resp := putIcon(t, s.URL, "admin-session", id, "sideways", pngBytes(t, 32, 32))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "unknown variant must be 400")
}

// A10 — GET sets an ETag; a conditional re-request with matching If-None-Match → 304.
func TestIconETagConditional304(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	id := firstServiceID(t, s.URL, "admin-session")
	putIcon(t, s.URL, "admin-session", id, "light", pngBytes(t, 48, 48)).Body.Close()

	got, _ := http.NewRequest(http.MethodGet, s.URL+"/api/services/"+id+"/icon/light", nil)
	got.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
	gr, err := http.DefaultClient.Do(got)
	require.NoError(t, err)
	etag := gr.Header.Get("ETag")
	gr.Body.Close()
	require.NotEmpty(t, etag, "GET must set an ETag")

	cond, _ := http.NewRequest(http.MethodGet, s.URL+"/api/services/"+id+"/icon/light", nil)
	cond.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
	cond.Header.Set("If-None-Match", etag)
	cr, err := http.DefaultClient.Do(cond)
	require.NoError(t, err)
	defer cr.Body.Close()
	assert.Equal(t, http.StatusNotModified, cr.StatusCode, "matching If-None-Match must be 304")
}

// A11 — DELETE is idempotent (204 with or without bytes); the variant then 404s.
func TestDeleteIconIdempotent(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	id := firstServiceID(t, s.URL, "admin-session")
	putIcon(t, s.URL, "admin-session", id, "dark", pngBytes(t, 48, 48)).Body.Close()

	del := func() int {
		req, _ := http.NewRequest(http.MethodDelete, s.URL+"/api/services/"+id+"/icon/dark", nil)
		req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
		r, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		r.Body.Close()
		return r.StatusCode
	}
	assert.Equal(t, http.StatusNoContent, del(), "DELETE existing must be 204")

	got, _ := http.NewRequest(http.MethodGet, s.URL+"/api/services/"+id+"/icon/dark", nil)
	got.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
	gr, _ := http.DefaultClient.Do(got)
	assert.Equal(t, http.StatusNotFound, gr.StatusCode, "deleted variant must 404")
	gr.Body.Close()

	assert.Equal(t, http.StatusNoContent, del(), "DELETE again must still be 204 (idempotent)")
}

// A12 — deleting a service cascades: no orphan service_icons rows remain.
func TestDeleteServiceCascadesIcons(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	id := firstServiceID(t, s.URL, "admin-session")
	putIcon(t, s.URL, "admin-session", id, "light", pngBytes(t, 48, 48)).Body.Close()
	putIcon(t, s.URL, "admin-session", id, "dark", pngBytes(t, 48, 48)).Body.Close()
	require.Equal(t, 2, countIconRows(t, id), "both icons should be stored before delete")

	req, _ := http.NewRequest(http.MethodDelete, s.URL+"/api/services/"+id, nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
	r, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	r.Body.Close()
	require.Equal(t, http.StatusNoContent, r.StatusCode)

	assert.Equal(t, 0, countIconRows(t, id), "service delete must cascade to service_icons (no orphans)")
}

// A13 — GET /api/services exposes iconLight/iconDark and never the blob bytes.
func TestListExposesIconFlagsNotBytes(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()
	id := firstServiceID(t, s.URL, "admin-session")
	putIcon(t, s.URL, "admin-session", id, "light", pngBytes(t, 256, 256)).Body.Close()

	req, _ := http.NewRequest(http.MethodGet, s.URL+"/api/services", nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Less(t, len(raw), 64*1024, "list response must not carry blob bytes")

	var payload struct {
		Services []struct {
			ID        string `json:"id"`
			IconLight bool   `json:"iconLight"`
			IconDark  bool   `json:"iconDark"`
		} `json:"services"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	var found bool
	for _, sv := range payload.Services {
		if sv.ID == id {
			found = true
			assert.True(t, sv.IconLight, "iconLight must be true after a light upload")
			assert.False(t, sv.IconDark, "iconDark must be false with no dark upload")
		}
	}
	assert.True(t, found, "uploaded service must appear in the list")
}

// countIconRows counts service_icons rows for a service id, connecting directly
// to the test Postgres — the authoritative check for the cascade AC.
func countIconRows(t *testing.T, serviceID string) int {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	require.NotEmpty(t, dsn)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close(ctx)
	var n int
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT count(*) FROM service_icons WHERE service_id = $1`, serviceID).Scan(&n))
	return n
}
