package storage

import (
	"net"
	"net/url"
	"strings"
	"testing"
)

// TestLibrarySeedIsPublicCatalog guards what every new install's App Library
// starts with. The seed ships in the public image and runs on any empty
// database, so it must be a catalog of public self-hosted apps, never someone's
// own homelab: until 2026-10-13 it was 45 entries on the maintainer's personal
// domain. Needs no database.
func TestLibrarySeedIsPublicCatalog(t *testing.T) {
	entries, err := parseSeed()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 20 {
		t.Fatalf("seed has %d entries; a useful App Library needs a real catalog", len(entries))
	}
	private := []string{".lan", ".local", ".internal", ".home", ".svc", ".cluster.local", "nip.io", "sslip.io", "localhost"}
	personal := []string{"calebdunn.tech"}
	for _, e := range entries {
		u, err := url.Parse(e.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			t.Errorf("%s: URL %q must be an https project URL", e.Name, e.URL)
			continue
		}
		h := strings.ToLower(u.Hostname())
		if net.ParseIP(h) != nil {
			t.Errorf("%s: IP-literal URL %q", e.Name, e.URL)
		}
		for _, s := range append(private, personal...) {
			if h == strings.TrimPrefix(s, ".") || strings.HasSuffix(h, s) || strings.HasSuffix(h, "."+strings.TrimPrefix(s, ".")) {
				t.Errorf("%s: %q is a private or personal host, not a public project URL", e.Name, e.URL)
			}
		}
		if u.User != nil || u.RawQuery != "" {
			t.Errorf("%s: URL %q carries userinfo or a query", e.Name, e.URL)
		}
	}
}
