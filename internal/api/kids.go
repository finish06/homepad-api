package api

import (
	"log"
	"net/http"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// SPEC-kid-accounts — the kid write lock and the filter helper the catalog
// handlers share.

// requireNotKid is the kid write lock (AC-007): one middleware, applied by
// route class in server.go to every kid-forbidden and admin-only route. A kid
// session gets 403 before the handler runs. It admits only the two adult
// roles, so an account whose role this code does not know is refused too
// (fail closed). A request with no live session passes through, so the
// handler answers 401 exactly as it did before.
func (s *server) requireNotKid(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u, ok := s.currentUser(w, r); ok && !storage.IsAdultRole(u.Role) {
			http.Error(w, "not available to kid accounts", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// visibleServices resolves what u may see: nil (everything) for an adult, the
// allow-list for a kid. On error it has written a 500 and returns false — a
// failed lookup never degrades to "show everything".
func (s *server) visibleServices(w http.ResponseWriter, r *http.Request, u storage.User) (*storage.ServiceSet, bool) {
	vis, err := s.store.VisibleServiceIDs(r.Context(), u)
	if err != nil {
		log.Printf("kid filter: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}
	return vis, true
}

// serviceVisible gates a per-service route on the caller's visibility: true
// when u may see serviceID. Otherwise it has written a 404 with notFoundMsg —
// the handler's own not-found message, so an off-list service is
// indistinguishable from a missing one (AC-006) — or a 500, and returns false.
func (s *server) serviceVisible(w http.ResponseWriter, r *http.Request, u storage.User, serviceID, notFoundMsg string) bool {
	vis, ok := s.visibleServices(w, r, u)
	if !ok {
		return false
	}
	if !vis.Allows(serviceID) {
		http.Error(w, notFoundMsg, http.StatusNotFound)
		return false
	}
	return true
}
