package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/gatus"
)

// fakeGatus serves a homelab-like Gatus: two demo endpoints plus private
// homelab endpoints whose names and URLs must never reach the snapshot.
func fakeGatus(t *testing.T) *httptest.Server {
	t.Helper()
	statuses := `[
	  {"key":"demo_grafana","name":"grafana","group":"demo","results":[{"success":true,"timestamp":"2026-10-13T20:00:00Z","duration":150000000,"hostname":"grafana.com"}]},
	  {"key":"core_nas","name":"nas","group":"core","results":[{"success":true,"timestamp":"2026-10-13T20:00:00Z","duration":1,"hostname":"nas.internal.lan"}]},
	  {"key":"demo_jellyfin","name":"jellyfin","group":"demo","results":[{"success":false,"timestamp":"2026-10-13T20:00:00Z","duration":0}]},
	  {"key":"friends_secret-box","name":"secret-box","group":"friends","results":[]}
	]`
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/endpoints/statuses", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(statuses))
	})
	mux.HandleFunc("GET /api/v1/endpoints/{key}/uptimes/{win}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("win") == "30d" && r.PathValue("key") == "demo_jellyfin" {
			http.NotFound(w, r) // best-effort: a missing window is skipped
			return
		}
		w.Write([]byte("0.99"))
	})
	return httptest.NewServer(mux)
}

func TestSnapshotPublishesOnlyDemoEndpoints(t *testing.T) {
	srv := fakeGatus(t)
	defer srv.Close()
	out := t.TempDir()

	n, err := snapshot(context.Background(), srv.Client(), srv.URL, out, "demo_")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("wrote %d endpoints, want 2", n)
	}

	// Nothing about the private homelab endpoints is anywhere in the output.
	filepath.Walk(out, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(p, "core_") || strings.Contains(p, "friends_") {
			t.Errorf("private endpoint path published: %s", p)
		}
		if !info.IsDir() {
			b, _ := os.ReadFile(p)
			for _, leak := range []string{"nas.internal.lan", "core_nas", "secret-box"} {
				if strings.Contains(string(b), leak) {
					t.Errorf("%s leaks %q", p, leak)
				}
			}
		}
		return nil
	})

	if _, err := os.Stat(filepath.Join(out, "api/v1/endpoints/demo_grafana/uptimes/7d")); err != nil {
		t.Errorf("uptime window missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "api/v1/endpoints/demo_jellyfin/uptimes/30d")); !os.IsNotExist(err) {
		t.Errorf("a window Gatus 404s must be omitted, not written (err=%v)", err)
	}
}

// The snapshot must be readable by homepad-api's real Gatus client, served as
// static files: this is exactly what the demo does with GATUS_BASE_URL.
func TestSnapshotIsReadableByHomepadGatusClient(t *testing.T) {
	srv := fakeGatus(t)
	defer srv.Close()
	out := t.TempDir()
	if _, err := snapshot(context.Background(), srv.Client(), srv.URL, out, "demo_"); err != nil {
		t.Fatal(err)
	}
	static := httptest.NewServer(http.FileServer(http.Dir(out)))
	defer static.Close()

	c := gatus.NewClient(static.URL)
	up, err := c.FetchUptime(context.Background(), "demo_grafana", "24h")
	if err != nil || up != 0.99 {
		t.Fatalf("FetchUptime from snapshot = %v, %v; want 0.99", up, err)
	}

	var got []struct {
		Key string `json:"key"`
	}
	resp, err := http.Get(static.URL + "/api/v1/endpoints/statuses")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "demo_grafana" || got[1].Key != "demo_jellyfin" {
		t.Fatalf("statuses = %+v", got)
	}
}

func TestSnapshotRefusesEmptyResult(t *testing.T) {
	srv := fakeGatus(t)
	defer srv.Close()
	out := t.TempDir()
	if _, err := snapshot(context.Background(), srv.Client(), srv.URL, out, "nomatch_"); err == nil {
		t.Fatal("an empty snapshot must be an error, so the last good one stays published")
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Fatalf("nothing may be written on refusal, got %d entries", len(entries))
	}
}

func TestSafeKey(t *testing.T) {
	for _, k := range []string{"demo_../../etc", "demo_a/b", `demo_a\b`, ""} {
		if safeKey(k) {
			t.Errorf("safeKey(%q) = true", k)
		}
	}
	if !safeKey("demo_uptime-kuma") {
		t.Error("a normal key must be accepted")
	}
}
