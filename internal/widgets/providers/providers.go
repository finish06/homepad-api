// Package providers holds the widget providers (SPEC-status-widgets §6.5).
// Each one talks to its app only through the guarded *widgets.Client, which
// a source test in this package enforces (TestProviders_UseOnlyTheGuardedClient).
//
// Phase 1: Jellyfin, Sonarr, Radarr. Phase 2 (Pi-hole, qBittorrent, Proxmox
// VE) adds files here and entries in Default; no schema or API change.
package providers

import (
	"net/http"
	"net/url"
	"strings"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/widgets"
)

// Default is the registry the server uses, in the order the editor lists
// the types.
func Default() *widgets.Registry {
	return widgets.NewRegistry(Jellyfin(), Sonarr(), Radarr())
}

// endpoint resolves path (and query) against the widget's address, keeping
// any path prefix the address has (e.g. http://host/jellyfin).
func endpoint(base *url.URL, path string, q url.Values) *url.URL {
	u := *base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawPath = ""
	u.RawQuery = ""
	if q != nil {
		u.RawQuery = q.Encode()
	}
	u.Fragment = ""
	return &u
}

func count(v float64) widgets.Value { return widgets.Value{Num: &v} }

func header(k, v string) http.Header {
	h := http.Header{}
	h.Set(k, v)
	return h
}
