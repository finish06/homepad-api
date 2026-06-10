package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// AC A11 — Gatus URL is never reachable from a browser session and never
// appears in any backend response payload. The frontend bundle-grep half of
// this AC lives in the homepad (web) repo.

const sentinelGatusURL = "http://gatus.10.17.2.213.nip.io"

func TestNoGatusURLInAnyResponse(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	endpoints := []string{
		"/api/services",
		"/api/status",
		"/api/me",
		"/health",
		"/live",
	}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, s.URL+ep, nil)
			req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "any-user"})
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			require.Equal(t, http.StatusOK, resp.StatusCode,
				"%s must return 200 so we can inspect the response body for the AC", ep)

			assert.False(t, strings.Contains(string(body), sentinelGatusURL),
				"%s response body must not contain the Gatus URL — leaked %q", ep, sentinelGatusURL)
		})
	}
}
