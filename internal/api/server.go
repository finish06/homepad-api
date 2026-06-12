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
	Sessions     *session.Manager
	Registration RegistrationMode
	OIDC         oidc.Config
}

type server struct {
	store        *storage.Store
	poller       *gatus.Poller
	sessions     *session.Manager
	registration RegistrationMode
	cookieSecure bool
	oidc         oidc.Config
	provider     *oidc.Provider
	pending      *oidc.Pending
}

func New(d Deps) http.Handler {
	s := &server{
		store:        d.Store,
		poller:       d.Poller,
		sessions:     d.Sessions,
		registration: d.Registration,
		cookieSecure: ParseCookieSecure(os.Getenv("COOKIE_SECURE")),
		oidc:         d.OIDC,
	}

	mux := http.NewServeMux()

	// Auth (A1) is the live vertical slice. Without a Store wired (e.g. the
	// scaffold test harness) these stay 501 so the surface is still routable.
	if d.Store != nil && d.Sessions != nil {
		mux.HandleFunc("POST /api/register", s.handleRegister)
		mux.HandleFunc("POST /api/login", s.handleLogin)
		mux.HandleFunc("POST /api/logout", s.handleLogout)
		mux.HandleFunc("GET /api/me", s.handleMe)
		mux.HandleFunc("PATCH /api/me", s.handlePatchMe)
		mux.HandleFunc("GET /api/me/collapsed-categories", s.handleGetCollapsedCategories)
		mux.HandleFunc("PUT /api/me/collapsed-categories", s.handleSetCollapsedCategories)
		mux.HandleFunc("GET /api/services", s.handleListServices)
		mux.HandleFunc("POST /api/services", s.handleCreateService)
		mux.HandleFunc("PATCH /api/services/{id}", s.handleUpdateService)
		mux.HandleFunc("DELETE /api/services/{id}", s.handleDeleteService)
		mux.HandleFunc("GET /api/categories", s.handleListCategories)
		mux.HandleFunc("POST /api/categories", s.handleCreateCategory)
		mux.HandleFunc("PUT /api/categories/order", s.handleSetCategoryOrder)
		mux.HandleFunc("PATCH /api/categories/{id}", s.handleUpdateCategory)
		mux.HandleFunc("DELETE /api/categories/{id}", s.handleDeleteCategory)
		mux.HandleFunc("GET /api/services/{id}/icon/{variant}", s.handleGetIcon)
		mux.HandleFunc("PUT /api/services/{id}/icon/{variant}", s.handlePutIcon)
		mux.HandleFunc("DELETE /api/services/{id}/icon/{variant}", s.handleDeleteIcon)
		mux.HandleFunc("POST /api/favorites/{id}", s.handleAddFavorite)
		mux.HandleFunc("DELETE /api/favorites/{id}", s.handleRemoveFavorite)
		mux.HandleFunc("PUT /api/layout", s.handleUpdateLayout)
		mux.HandleFunc("GET /api/status", s.handleStatus)
		mux.HandleFunc("GET /health", s.handleHealth)

		// OIDC config is always advertised so the web client can gate its
		// "Log in with PocketID" button. The login/callback endpoints exist
		// only when OIDC is enabled — otherwise they stay unregistered (404)
		// and homepad behaves as a local-accounts-only app.
		mux.HandleFunc("GET /api/auth/config", s.handleOIDCConfig)
		if d.OIDC.Enabled {
			s.provider = oidc.NewProvider(d.OIDC)
			s.pending = oidc.NewPending(oidcPendingTTL)
			mux.HandleFunc("GET /api/auth/oidc/login", s.handleOIDCLogin)
			mux.HandleFunc("GET /api/auth/oidc/callback", s.handleOIDCCallback)
		}
	} else {
		for _, p := range []string{"POST /api/register", "POST /api/login", "POST /api/logout", "GET /api/me", "PATCH /api/me", "GET /api/me/collapsed-categories", "PUT /api/me/collapsed-categories", "GET /api/services", "POST /api/services", "PATCH /api/services/{id}", "DELETE /api/services/{id}", "GET /api/categories", "POST /api/categories", "PUT /api/categories/order", "PATCH /api/categories/{id}", "DELETE /api/categories/{id}", "GET /api/services/{id}/icon/{variant}", "PUT /api/services/{id}/icon/{variant}", "DELETE /api/services/{id}/icon/{variant}", "POST /api/favorites/{id}", "DELETE /api/favorites/{id}", "PUT /api/layout", "GET /api/status", "GET /health"} {
			mux.HandleFunc(p, notImplemented)
		}
	}

	mux.HandleFunc("GET /live", handleLive)

	return mux
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
