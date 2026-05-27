package testsupport

import (
	"net/http/httptest"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
)

// NewServer returns an httptest.Server running the real homepad-api handler
// with no business logic wired in (scaffold/RED phase). Callers must Close().
func NewServer() *httptest.Server {
	return httptest.NewServer(api.New(api.Deps{}))
}
