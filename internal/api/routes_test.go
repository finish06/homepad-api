package api_test

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/oidc"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
)

// SPEC-kid-accounts §7 / plan TASK-205 — the route-coverage test.
//
// The risk this guards against is a catalog route that misses the kid filter
// and leaks a hidden app. A comment saying "every catalog read goes through
// VisibleServiceIDs" ages silently; this test enforces it instead:
//
//  1. The route list comes from the REAL registration in server.go (every
//     route is registered through router.handle, which records it), not from a
//     copy here. TestRoutes_OnlyRegisteredThroughRouter proves nothing in the
//     package registers a handler any other way.
//  2. Every registered route must appear in expectedRouteClasses below, with
//     the same class server.go gives it. A new route that is not classified
//     here fails the build, as does a reclassification nobody reviewed.
//  3. Every class is then PROBED with a real kid session against a catalog
//     holding hidden apps: forbidden routes must 403, filtered routes must not
//     leak, and no route at all may return a hidden app's id, name, URL, Gatus
//     key or category in its body.
//
// Classes:
//
//	public        — no session needed (auth bootstrap, health, public config).
//	catalog-free  — session-gated, returns or writes no catalog data; kids allowed.
//	kid-filtered  — returns or touches catalog data; filtered through
//	                storage.VisibleServiceIDs for a kid (off-list → absent / 404).
//	kid-forbidden — open to adults (admin and user), 403 for a kid (the
//	                requireNotKid middleware, applied by class in server.go).
//	admin-only    — 403 for anyone but an admin (requireAdmin in the handler,
//	                and requireNotKid in front of it).
//	catalog-write — admin-only, and the write acts on the shared catalog
//	                owner's rows for every admin (SPEC-household-basics
//	                AC-023). second_admin_test.go probes each one as a
//	                second admin (AC-025).
var expectedRouteClasses = map[string]string{
	// Auth bootstrap and public config.
	"POST /api/register":          "public",
	"POST /api/login":             "public",
	"GET /api/auth/config":        "public",
	"GET /api/auth/oidc/login":    "public",
	"GET /api/auth/oidc/callback": "public",
	"GET /api/system/config":      "public",
	"GET /health":                 "public",
	"GET /live":                   "public",
	// SPEC-household-basics §7 — the invite link's two public, rate-limited routes.
	"POST /api/invites/lookup":         "public",
	"POST /api/invites/accept":         "public",
	"POST /api/logout":                 "catalog-free",
	"GET /api/me":                      "catalog-free",
	"PATCH /api/me":                    "catalog-free", // a kid may send themePref only (handler-enforced, AC-007)
	"GET /api/me/collapsed-categories": "kid-filtered",
	"PUT /api/me/collapsed-categories": "kid-forbidden",

	// Catalog reads (AC-003…AC-006).
	"GET /api/services":                     "kid-filtered",
	"GET /api/categories":                   "kid-filtered",
	"GET /api/status":                       "kid-filtered",
	"GET /api/services/{id}/icon/{variant}": "kid-filtered",
	"POST /api/favorites/{id}":              "kid-filtered",
	"DELETE /api/favorites/{id}":            "kid-filtered",

	// Adult-only, not admin-only.
	"GET /api/library":         "kid-forbidden",
	"PUT /api/layout":          "kid-forbidden",
	"POST /api/status/refresh": "kid-forbidden",

	// Catalog writes: admin-only, acting on the shared catalog (#87).
	"POST /api/services":                       "catalog-write",
	"PATCH /api/services/{id}":                 "catalog-write",
	"DELETE /api/services/{id}":                "catalog-write",
	"POST /api/categories":                     "catalog-write",
	"PUT /api/categories/order":                "catalog-write",
	"PATCH /api/categories/{id}":               "catalog-write",
	"DELETE /api/categories/{id}":              "catalog-write",
	"PUT /api/services/{id}/icon/{variant}":    "catalog-write",
	"DELETE /api/services/{id}/icon/{variant}": "catalog-write",
	"POST /api/services/{id}/fetch-icon":       "catalog-write",
	"POST /api/library/{id}/add":               "catalog-write",

	// Library, admin and settings mutations.
	"POST /api/library":         "admin-only",
	"PUT /api/library/order":    "admin-only",
	"PATCH /api/library/{id}":   "admin-only",
	"DELETE /api/library/{id}":  "admin-only",
	"PATCH /api/admin/settings": "admin-only",
	"GET /api/admin/env-config": "admin-only",

	// The admin Accounts API (SPEC-kid-accounts §7, plan TASK-305).
	"GET /api/admin/accounts":                    "admin-only",
	"POST /api/admin/kids":                       "admin-only",
	"PATCH /api/admin/kids/{id}":                 "admin-only",
	"DELETE /api/admin/kids/{id}":                "admin-only",
	"GET /api/admin/kids/{id}/services":          "admin-only",
	"PUT /api/admin/kids/{id}/services":          "admin-only",
	"GET /api/admin/kids/{id}/sessions":          "admin-only",
	"DELETE /api/admin/kids/{id}/sessions/{sid}": "admin-only",
	"POST /api/admin/kids/{id}/sign-in-here":     "admin-only",

	// Invites (SPEC-household-basics §7).
	"POST /api/admin/invites":        "admin-only",
	"GET /api/admin/invites":         "admin-only",
	"DELETE /api/admin/invites/{id}": "admin-only",

	// Backup, restore and Homepage import (SPEC-backup-import §8).
	"GET /api/admin/export":                   "admin-only",
	"POST /api/admin/restore/preview":         "admin-only",
	"POST /api/admin/restore":                 "admin-only",
	"POST /api/admin/import/homepage/preview": "admin-only",
	"POST /api/admin/import/homepage":         "admin-only",
}

// allRoutes is the full registration: OIDC on (so its two routes register)
// and no store (registration does not depend on one).
func allRoutes(t *testing.T) []api.RouteInfo {
	t.Helper()
	return api.RegisteredRoutes(api.Deps{OIDC: oidc.Config{Enabled: true, Issuer: "http://127.0.0.1:1", ClientID: "x"}})
}

func TestRoutes_EveryRegisteredRouteIsClassified(t *testing.T) {
	registered := allRoutes(t)
	require.NotEmpty(t, registered)

	seen := map[string]bool{}
	for _, r := range registered {
		assert.False(t, seen[r.Pattern], "route %q is registered twice", r.Pattern)
		seen[r.Pattern] = true
		want, ok := expectedRouteClasses[r.Pattern]
		if !assert.True(t, ok,
			"route %q (class %q in server.go) is not classified in expectedRouteClasses. "+
				"Decide whether a kid may reach it and whether it returns catalog data, "+
				"classify it in server.go AND here, and probe it below.", r.Pattern, r.Class) {
			continue
		}
		assert.Equal(t, want, r.Class, "route %q: server.go classifies it %q, the review table says %q", r.Pattern, r.Class, want)
	}
	for p := range expectedRouteClasses {
		assert.True(t, seen[p], "expectedRouteClasses lists %q, which server.go does not register", p)
	}
}

// The store-backed server registers the same routes (minus OIDC when it is
// off): one table drives both modes, so there is no second list to drift.
func TestRoutes_StoreBackedRegistrationMatches(t *testing.T) {
	w := newKidWorld(t)
	live := api.RegisteredRoutes(api.Deps{Store: w.env.Store, Sessions: w.env.Sessions})
	var got, want []string
	for _, r := range live {
		got = append(got, r.Pattern)
	}
	for _, r := range allRoutes(t) {
		if !strings.Contains(r.Pattern, "/api/auth/oidc/") {
			want = append(want, r.Pattern)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	assert.Equal(t, want, got)
}

// Nothing in package api may register a handler except router.handle, so the
// recorded list IS the mux's list.
func TestRoutes_OnlyRegisteredThroughRouter(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
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
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			inRouter := fn.Name.Name == "handle" && fn.Recv != nil && len(fn.Recv.List) == 1 &&
				exprString(fn.Recv.List[0].Type) == "*router"
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if (sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc") && !inRouter {
					t.Errorf("%s: %s.%s called in %s — register routes only via router.handle so the route-coverage test sees them",
						fset.Position(call.Pos()), exprString(sel.X), sel.Sel.Name, fn.Name.Name)
				}
				return true
			})
		}
	}
	require.NotZero(t, checked, "no source files were checked")
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		return "*" + exprString(v.X)
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	default:
		return fmt.Sprintf("%T", e)
	}
}

// concretePath fills a pattern's wildcards with ids from the kid world, always
// choosing the HIDDEN service / category where there is a choice, so a filtered
// route that forgot to filter would serve hidden data here.
func concretePath(w kidWorld, offerID, pattern string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	switch {
	case strings.HasPrefix(path, "/api/services/{id}"), strings.HasPrefix(path, "/api/favorites/{id}"):
		path = strings.Replace(path, "{id}", w.hiddenID, 1)
	case strings.HasPrefix(path, "/api/categories/{id}"):
		path = strings.Replace(path, "{id}", w.secretCat, 1)
	case strings.HasPrefix(path, "/api/library/{id}"):
		path = strings.Replace(path, "{id}", offerID, 1)
	case strings.HasPrefix(path, "/api/admin/kids/{id}"):
		path = strings.Replace(path, "{id}", w.kidID, 1)
	case strings.HasPrefix(path, "/api/admin/invites/{id}"):
		path = strings.Replace(path, "{id}", "00000000-0000-0000-0000-000000000000", 1)
	}
	path = strings.Replace(path, "{variant}", "light", 1)
	path = strings.Replace(path, "{sid}", "0000", 1)
	return method, path
}

const muxNotFound = "404 page not found\n"

func TestRoutes_KidProbeOfEveryRoute(t *testing.T) {
	w := newKidWorld(t)
	offer := createOffer(t, w.env.URL, "admin-session", "Plex", "https://plex.example.com")
	// The hidden service has an uploaded icon, so an unfiltered icon read would
	// actually serve bytes.
	r := putIcon(t, w.env.URL, "admin-session", w.hiddenID, "light", pngBytes(t, 32, 32))
	r.Body.Close()
	require.Equal(t, http.StatusNoContent, r.StatusCode)

	live := map[string]bool{}
	for _, rt := range api.RegisteredRoutes(api.Deps{Store: w.env.Store, Sessions: w.env.Sessions}) {
		live[rt.Pattern] = true
	}

	ctx := context.Background()
	n := 0
	for _, rt := range allRoutes(t) {
		rt := rt
		if !live[rt.Pattern] {
			t.Logf("skip %s: not registered with OIDC off (public, covered by oidc_test.go)", rt.Pattern)
			continue
		}
		n++
		t.Run(rt.Pattern, func(t *testing.T) {
			method, path := concretePath(w, offer.ID, rt.Pattern)

			// A fresh kid session per probe, so POST /api/logout cannot sign
			// out the others.
			tok := fmt.Sprintf("kid-probe-%d-%s", n, w.kidID)
			require.NoError(t, w.env.Sessions.Bind(ctx, tok, w.kidID, session.KidLifetime))

			resp := doJSON(t, method, w.env.URL+path, tok, nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			assert.NotEqual(t, http.StatusMethodNotAllowed, resp.StatusCode, "the probe must reach the route")
			assert.NotEqual(t, muxNotFound, string(body), "the probe must reach the handler, not the mux 404")
			assertNoHiddenMarkers(t, w, rt.Pattern, body)

			switch rt.Class {
			case "kid-forbidden", "admin-only", "catalog-write":
				assert.Equal(t, http.StatusForbidden, resp.StatusCode, "%s is %s: a kid gets 403", rt.Pattern, rt.Class)
			case "kid-filtered", "catalog-free":
				assert.NotEqual(t, http.StatusForbidden, resp.StatusCode, "%s is %s: a kid may call it", rt.Pattern, rt.Class)
				assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode, "%s: the kid session is valid", rt.Pattern)
				if strings.Contains(rt.Pattern, "{id}") && rt.Class == "kid-filtered" {
					assert.Equal(t, http.StatusNotFound, resp.StatusCode, "%s with a hidden id is 404", rt.Pattern)
				}
			case "public":
				anon := doNoCookie(t, method, w.env.URL+path, nil)
				anon.Body.Close()
				assert.NotEqual(t, http.StatusUnauthorized, anon.StatusCode, "%s is public", rt.Pattern)
			default:
				t.Fatalf("unknown class %q", rt.Class)
			}

			// The class is not stricter than it needs to be: kid-forbidden is
			// open to a plain user, admin-only is not.
			switch rt.Class {
			case "kid-forbidden":
				ur := doJSON(t, method, w.env.URL+path, "non-admin-session", nil)
				ur.Body.Close()
				assert.NotEqual(t, http.StatusForbidden, ur.StatusCode, "%s is open to plain users", rt.Pattern)
			case "admin-only", "catalog-write":
				ur := doJSON(t, method, w.env.URL+path, "non-admin-session", nil)
				ur.Body.Close()
				assert.Equal(t, http.StatusForbidden, ur.StatusCode, "%s is admin-only", rt.Pattern)
			}
		})
	}
	require.Greater(t, n, 30, "the probe covered the registered routes")
}
