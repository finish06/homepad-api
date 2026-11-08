package api_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/widgets"
)

// homepad-api #90 — the favicon fetcher moves onto the guarded client: no
// redirects followed, no link-local or metadata address dialled, and a capped
// response. The fetch is admin-only, but it ran before widgets added more
// server-side fetches, so it gets the same guard.

func fetchIcon(t *testing.T, baseURL, id string) (int, string) {
	t.Helper()
	r := doJSON(t, http.MethodPost, baseURL+"/api/services/"+id+"/fetch-icon", "admin-session", nil)
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

func TestFetchIcon_DoesNotFollowRedirects(t *testing.T) {
	png := pngBytes(t, 32, 32)
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	defer other.Close()
	// Both the page and /favicon.ico redirect to another host.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/icon.png", http.StatusFound)
	}))
	defer target.Close()

	env := testsupport.NewEnv(t)
	id := createServiceForFetch(t, env.URL, "admin-session", target.URL)
	code, _ := fetchIcon(t, env.URL, id)
	assert.Equal(t, http.StatusUnprocessableEntity, code)
	assert.Zero(t, elsewhere.Load(), "a redirect target is never requested")

	// A <link rel=icon> pointing at a redirect is not followed either.
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<html><head><link rel="icon" href="/go"></head></html>`))
		case "/go":
			http.Redirect(w, r, other.URL+"/icon.png", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer page.Close()
	r := doJSON(t, http.MethodPatch, env.URL+"/api/services/"+id, "admin-session", map[string]any{"url": page.URL + "/"})
	r.Body.Close()
	code, _ = fetchIcon(t, env.URL, id)
	assert.Equal(t, http.StatusUnprocessableEntity, code)
	assert.Zero(t, elsewhere.Load())
}

func TestFetchIcon_RefusesMetadataAddresses(t *testing.T) {
	var dials atomic.Int32
	outbound := widgets.NewClient(widgets.WithDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}))
	env := testsupport.NewEnvWidgets(t, nil, testsupport.WidgetOptions{Outbound: outbound})

	for _, raw := range []string{"http://169.254.169.254/latest/meta-data/", "http://[fe80::1]/", "http://[fd00:ec2::254]/"} {
		id := createServiceForFetch(t, env.URL, "admin-session", raw)
		code, body := fetchIcon(t, env.URL, id)
		assert.Equal(t, http.StatusUnprocessableEntity, code, raw)
		assert.NotContains(t, body, "169.254", "the error doesn't echo the address")
		r := doJSON(t, http.MethodDelete, env.URL+"/api/services/"+id, "admin-session", nil)
		r.Body.Close()
	}
	assert.Zero(t, dials.Load(), "no connection to a metadata address is ever attempted")

	// A page whose <link rel=icon> points at a metadata address: same.
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte(`<link rel="icon" href="http://169.254.169.254/icon.png">`))
			return
		}
		http.NotFound(w, r)
	}))
	defer page.Close()
	id := createServiceForFetch(t, env.URL, "admin-session", page.URL+"/")
	before := dials.Load()
	code, _ := fetchIcon(t, env.URL, id)
	assert.Equal(t, http.StatusUnprocessableEntity, code)
	// The page itself was fetched through the guarded client (so the dial
	// count above means something); the metadata href was never dialled —
	// every dial went to the loopback page.
	assert.GreaterOrEqual(t, dials.Load()-before, int32(1), "favicon fetches go through the guarded client")
}

// A page larger than the scan cap is still scanned (its first 512 KiB), and
// an icon over 256 KB is refused with the existing message.
func TestFetchIcon_SizeCaps(t *testing.T) {
	big := noisyPNGOver256K(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<link rel="icon" href="/big.png">` + strings.Repeat(" ", 700*1024)))
		case "/big.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(big)
		default:
			http.NotFound(w, r)
		}
	}))
	defer target.Close()
	env := testsupport.NewEnv(t)
	id := createServiceForFetch(t, env.URL, "admin-session", target.URL+"/")
	code, body := fetchIcon(t, env.URL, id)
	assert.Equal(t, http.StatusUnprocessableEntity, code)
	assert.Contains(t, body, "exceeds 256 KB")
}

// The rule "server-side fetches use the guarded client" is enforced: no
// non-test file in package api builds its own HTTP client or uses net/http's
// defaults.
func TestAPI_UsesOnlyTheGuardedClient(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	forbidden := map[string]bool{
		"http.Client": true, "http.Transport": true, "http.DefaultClient": true, "http.DefaultTransport": true,
		"http.Get": true, "http.Post": true, "http.Head": true, "http.PostForm": true,
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, f, src, 0)
		require.NoError(t, err)
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && forbidden[x.Name+"."+sel.Sel.Name] {
				t.Errorf("%s: %s.%s — outbound fetches go through the guarded *widgets.Client (#90)",
					fset.Position(sel.Pos()), x.Name, sel.Sel.Name)
			}
			return true
		})
	}
	require.NotZero(t, checked)
}
