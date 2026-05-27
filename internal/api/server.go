package api

import (
	"net/http"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/gatus"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

type Deps struct {
	Store        *storage.Store
	Poller       *gatus.Poller
	Sessions     *session.Manager
	Registration string
}

func New(d Deps) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/register", notImplemented)
	mux.HandleFunc("POST /api/login", notImplemented)
	mux.HandleFunc("POST /api/logout", notImplemented)
	mux.HandleFunc("GET /api/me", notImplemented)

	mux.HandleFunc("GET /api/services", notImplemented)
	mux.HandleFunc("GET /api/status", notImplemented)

	mux.HandleFunc("POST /api/services", notImplemented)
	mux.HandleFunc("PATCH /api/services/{id}", notImplemented)
	mux.HandleFunc("DELETE /api/services/{id}", notImplemented)

	mux.HandleFunc("POST /api/favorites/{id}", notImplemented)
	mux.HandleFunc("DELETE /api/favorites/{id}", notImplemented)
	mux.HandleFunc("PUT /api/layout", notImplemented)

	mux.HandleFunc("GET /health", notImplemented)
	mux.HandleFunc("GET /live", notImplemented)

	return mux
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
