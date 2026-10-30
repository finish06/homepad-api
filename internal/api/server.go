package api

import (
	"net/http"
	"os"
	"time"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/gatus"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/oidc"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

const sessionCookie = "homepad_session"

// oidcPendingTTL bounds how long a login may sit between the authorize redirect
// and the callback.
const oidcPendingTTL = 10 * time.Minute

type Deps struct {
	Store        *storage.Store
	Poller       *gatus.Poller
	Sessions     session.Store
	Registration RegistrationMode
	OIDC         oidc.Config
}

type server struct {
	store        *storage.Store
	poller       *gatus.Poller
	sessions     session.Store
	registration RegistrationMode
	cookieSecure bool
	oidc         oidc.Config
	provider     *oidc.Provider
	pending      *oidc.Pending
}

func New(d Deps) http.Handler {
	h, _ := newServer(d).routes(d)
	return h
}

func newServer(d Deps) *server {
	return &server{
		store:        d.Store,
		poller:       d.Poller,
		sessions:     d.Sessions,
		registration: d.Registration,
		cookieSecure: ParseCookieSecure(os.Getenv("COOKIE_SECURE")),
		oidc:         d.OIDC,
	}
}

// routeClass is what a route means for a kid session (SPEC-kid-accounts §7).
// Every route is registered with one, and the class drives enforcement: the
// kid write lock is applied here, by class, not remembered per handler. The
// route-coverage test (routes_test.go) holds an independent copy of every
// route's class and probes each one with a kid session.
type routeClass string

const (
	// classPublic: no session needed (auth bootstrap, health, public config).
	classPublic routeClass = "public"
	// classCatalogFree: session-gated, returns or writes no catalog data; a
	// kid may call it (PATCH /api/me limits a kid to themePref itself).
	classCatalogFree routeClass = "catalog-free"
	// classKidFiltered: returns or touches catalog data; the handler filters
	// through storage.VisibleServiceIDs for a kid.
	classKidFiltered routeClass = "kid-filtered"
	// classKidForbidden: open to adults, 403 for a kid (requireNotKid).
	classKidForbidden routeClass = "kid-forbidden"
	// classAdminOnly: requireAdmin in the handler, and requireNotKid in front
	// of it, so a kid is refused even if a handler forgot its admin gate.
	classAdminOnly routeClass = "admin-only"
)

// router is the only place a route is registered (routes_test.go checks that
// nothing else calls Handle/HandleFunc), so its record is the mux's route list.
type router struct {
	s      *server
	mux    *http.ServeMux
	routes []registeredRoute
	// stub replaces every handler with a 501 (the no-store scaffold mode).
	stub bool
}

type registeredRoute struct {
	pattern string
	class   routeClass
}

// livePattern is the liveness probe, the one route that answers even in the
// no-store scaffold mode.
const livePattern = "GET /live"

func (rt *router) handle(pattern string, class routeClass, h http.HandlerFunc) {
	var handler http.Handler = h
	switch {
	case rt.stub && pattern != livePattern:
		handler = http.HandlerFunc(notImplemented)
	case class == classKidForbidden || class == classAdminOnly:
		handler = rt.s.requireNotKid(h)
	}
	rt.mux.Handle(pattern, handler)
	rt.routes = append(rt.routes, registeredRoute{pattern: pattern, class: class})
}

// routes registers every route and returns the handler with the record of
// what was registered. Without a Store wired (e.g. the scaffold test harness)
// the same routes are registered as 501s, so the surface is still routable and
// there is no second, hand-copied list to drift.
func (s *server) routes(d Deps) (http.Handler, []registeredRoute) {
	rt := &router{s: s, mux: http.NewServeMux(), stub: d.Store == nil || d.Sessions == nil}

	rt.handle("POST /api/register", classPublic, s.handleRegister)
	rt.handle("POST /api/login", classPublic, s.handleLogin)
	rt.handle("POST /api/logout", classCatalogFree, s.handleLogout)
	rt.handle("GET /api/me", classCatalogFree, s.handleMe)
	rt.handle("PATCH /api/me", classCatalogFree, s.handlePatchMe)
	rt.handle("GET /api/me/collapsed-categories", classKidFiltered, s.handleGetCollapsedCategories)
	rt.handle("PUT /api/me/collapsed-categories", classKidForbidden, s.handleSetCollapsedCategories)
	rt.handle("GET /api/services", classKidFiltered, s.handleListServices)
	rt.handle("POST /api/services", classAdminOnly, s.handleCreateService)
	rt.handle("PATCH /api/services/{id}", classAdminOnly, s.handleUpdateService)
	rt.handle("DELETE /api/services/{id}", classAdminOnly, s.handleDeleteService)
	rt.handle("GET /api/categories", classKidFiltered, s.handleListCategories)
	rt.handle("POST /api/categories", classAdminOnly, s.handleCreateCategory)
	rt.handle("PUT /api/categories/order", classAdminOnly, s.handleSetCategoryOrder)
	rt.handle("PATCH /api/categories/{id}", classAdminOnly, s.handleUpdateCategory)
	rt.handle("DELETE /api/categories/{id}", classAdminOnly, s.handleDeleteCategory)
	rt.handle("GET /api/services/{id}/icon/{variant}", classKidFiltered, s.handleGetIcon)
	rt.handle("PUT /api/services/{id}/icon/{variant}", classAdminOnly, s.handlePutIcon)
	rt.handle("DELETE /api/services/{id}/icon/{variant}", classAdminOnly, s.handleDeleteIcon)
	rt.handle("POST /api/services/{id}/fetch-icon", classAdminOnly, s.handleFetchIcon)
	rt.handle("POST /api/favorites/{id}", classKidFiltered, s.handleAddFavorite)
	rt.handle("DELETE /api/favorites/{id}", classKidFiltered, s.handleRemoveFavorite)
	// App Library (v9.2): browse is session-gated (any adult); CRUD, reorder
	// and add are admin-gated via requireAdmin (D10, SPEC-245-224). /order is
	// declared before /{id} so the literal path wins the ServeMux match.
	rt.handle("GET /api/library", classKidForbidden, s.handleListLibrary)
	rt.handle("POST /api/library", classAdminOnly, s.handleCreateLibraryApp)
	rt.handle("PUT /api/library/order", classAdminOnly, s.handleSetLibraryOrder)
	rt.handle("PATCH /api/library/{id}", classAdminOnly, s.handleUpdateLibraryApp)
	rt.handle("DELETE /api/library/{id}", classAdminOnly, s.handleDeleteLibraryApp)
	rt.handle("POST /api/library/{id}/add", classAdminOnly, s.handleAddFromLibrary)
	rt.handle("PUT /api/layout", classKidForbidden, s.handleUpdateLayout)
	rt.handle("GET /api/status", classKidFiltered, s.handleStatus)
	// SPEC-v24 §12.3 — on-demand re-poll behind the health panel's "Retry now".
	rt.handle("POST /api/status/refresh", classKidForbidden, s.handleStatusRefresh)
	rt.handle("GET /health", classPublic, s.handleHealth)

	// System settings (cap6). The config read is public — registered
	// alongside GET /api/auth/config as the two unauthenticated config
	// endpoints (D4) — while the write is admin-only via requireAdmin (D5).
	rt.handle("GET /api/system/config", classPublic, s.handleSystemConfig)
	rt.handle("PATCH /api/admin/settings", classAdminOnly, s.handlePatchSystemSettings)
	// SPEC-v26 — read-only admin view of allowlisted runtime env vars.
	rt.handle("GET /api/admin/env-config", classAdminOnly, s.handleAdminEnvConfig)

	// SPEC-kid-accounts §7 — the admin Accounts API.
	rt.handle("GET /api/admin/accounts", classAdminOnly, s.handleListAccounts)
	rt.handle("POST /api/admin/kids", classAdminOnly, s.handleCreateKid)
	rt.handle("PATCH /api/admin/kids/{id}", classAdminOnly, s.handleUpdateKid)
	rt.handle("DELETE /api/admin/kids/{id}", classAdminOnly, s.handleDeleteKid)
	rt.handle("GET /api/admin/kids/{id}/services", classAdminOnly, s.handleGetKidServices)
	rt.handle("PUT /api/admin/kids/{id}/services", classAdminOnly, s.handleSetKidServices)
	rt.handle("GET /api/admin/kids/{id}/sessions", classAdminOnly, s.handleListKidSessions)
	rt.handle("DELETE /api/admin/kids/{id}/sessions/{sid}", classAdminOnly, s.handleRevokeKidSession)
	rt.handle("POST /api/admin/kids/{id}/sign-in-here", classAdminOnly, s.handleSignInHere)

	// OIDC config is always advertised so the web client can gate its
	// "Log in with PocketID" button. The login/callback endpoints exist
	// only when OIDC is enabled — otherwise they stay unregistered (404)
	// and homepad behaves as a local-accounts-only app.
	rt.handle("GET /api/auth/config", classPublic, s.handleOIDCConfig)
	if d.OIDC.Enabled {
		s.provider = oidc.NewProvider(d.OIDC)
		s.pending = oidc.NewPending(oidcPendingTTL)
		rt.handle("GET /api/auth/oidc/login", classPublic, s.handleOIDCLogin)
		rt.handle("GET /api/auth/oidc/callback", classPublic, s.handleOIDCCallback)
	}

	rt.handle(livePattern, classPublic, handleLive)

	return rt.mux, rt.routes
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleLive(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
