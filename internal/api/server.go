package api

import (
	"net/http"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/gatus"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

const sessionCookie = "homepad_session"

type Deps struct {
	Store        *storage.Store
	Poller       *gatus.Poller
	Sessions     *session.Manager
	Registration string
}

type server struct {
	store        *storage.Store
	poller       *gatus.Poller
	sessions     *session.Manager
	registration string
}

func New(d Deps) http.Handler {
	s := &server{
		store:        d.Store,
		poller:       d.Poller,
		sessions:     d.Sessions,
		registration: d.Registration,
	}

	mux := http.NewServeMux()

	// Auth (A1) is the live vertical slice. Without a Store wired (e.g. the
	// scaffold test harness) these stay 501 so the surface is still routable.
	if d.Store != nil && d.Sessions != nil {
		mux.HandleFunc("POST /api/register", s.handleRegister)
		mux.HandleFunc("POST /api/login", s.handleLogin)
		mux.HandleFunc("POST /api/logout", s.handleLogout)
		mux.HandleFunc("GET /api/me", s.handleMe)
		mux.HandleFunc("GET /api/services", s.handleListServices)
		mux.HandleFunc("POST /api/services", s.handleCreateService)
		mux.HandleFunc("GET /api/status", s.handleStatus)
		mux.HandleFunc("GET /health", s.handleHealth)
	} else {
		for _, p := range []string{"POST /api/register", "POST /api/login", "POST /api/logout", "GET /api/me", "GET /api/services", "POST /api/services", "GET /api/status", "GET /health"} {
			mux.HandleFunc(p, notImplemented)
		}
	}

	// Still stubbed — catalog edit/delete, favorites, layout land in later slices.
	mux.HandleFunc("PATCH /api/services/{id}", notImplemented)
	mux.HandleFunc("DELETE /api/services/{id}", notImplemented)
	mux.HandleFunc("POST /api/favorites/{id}", notImplemented)
	mux.HandleFunc("DELETE /api/favorites/{id}", notImplemented)
	mux.HandleFunc("PUT /api/layout", notImplemented)

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
