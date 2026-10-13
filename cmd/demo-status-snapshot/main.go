// Command demo-status-snapshot gives the public demo (demo.gethomepad.dev) live
// status without running a checker inside it.
//
// The demo is Cloud Run with request-scoped CPU and scale-to-zero, so a Gatus
// beside it would have run none of its checks by the time a cold visitor looks
// (homepad-api #76). Instead the homelab Gatus checks the demo tiles' public
// project URLs (deploy/demo/gatus-endpoints.json, group "demo"), and this
// command, run on a schedule in the homelab, writes a static copy of exactly
// the two Gatus API paths homepad-api reads:
//
//	<out>/api/v1/endpoints/statuses
//	<out>/api/v1/endpoints/<key>/uptimes/<window>   (24h, 7d, 30d)
//
// Upload <out> to a public bucket and set the demo api's GATUS_BASE_URL to it.
//
// Only keys starting with -prefix (default "demo_") are written. Everything else
// the homelab Gatus monitors is dropped before anything touches disk: this
// snapshot is published to the internet, and the homelab's own endpoint names
// and URLs must never be in it.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Windows homepad-api requests (internal/gatus.UptimeWindows).
var uptimeWindows = []string{"24h", "7d", "30d"}

func main() {
	gatusURL := flag.String("gatus", "", "base URL of the Gatus to snapshot (required)")
	out := flag.String("out", "", "directory to write the snapshot into (required)")
	prefix := flag.String("prefix", "demo_", "only endpoint keys with this prefix are published")
	flag.Parse()
	if *gatusURL == "" || *out == "" || *prefix == "" {
		flag.Usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	n, err := snapshot(ctx, &http.Client{Timeout: 20 * time.Second}, strings.TrimRight(*gatusURL, "/"), *out, *prefix)
	if err != nil {
		log.Fatalf("demo-status-snapshot: %v", err)
	}
	log.Printf("demo-status-snapshot: wrote %d endpoints to %s", n, *out)
}

// snapshot writes the filtered statuses and per-key uptimes. It refuses to write
// an empty snapshot: if no key matches, something is misconfigured, and keeping
// the last good snapshot is better than publishing a board of UNKNOWNs.
func snapshot(ctx context.Context, hc *http.Client, base, out, prefix string) (int, error) {
	body, err := get(ctx, hc, base+"/api/v1/endpoints/statuses")
	if err != nil {
		return 0, err
	}
	var all []map[string]json.RawMessage
	if err := json.Unmarshal(body, &all); err != nil {
		return 0, fmt.Errorf("parse statuses: %w", err)
	}
	var kept []map[string]json.RawMessage
	var keys []string
	for _, ep := range all {
		var key string
		if err := json.Unmarshal(ep["key"], &key); err != nil || !strings.HasPrefix(key, prefix) || !safeKey(key) {
			continue
		}
		kept = append(kept, ep)
		keys = append(keys, key)
	}
	if len(kept) == 0 {
		return 0, fmt.Errorf("no endpoint key starts with %q; refusing to publish an empty snapshot", prefix)
	}
	statuses, err := json.Marshal(kept)
	if err != nil {
		return 0, err
	}
	if err := write(filepath.Join(out, "api/v1/endpoints/statuses"), statuses); err != nil {
		return 0, err
	}
	for _, key := range keys {
		for _, win := range uptimeWindows {
			v, err := get(ctx, hc, base+"/api/v1/endpoints/"+key+"/uptimes/"+win)
			if err != nil {
				// Best-effort, like homepad-api's own fillUptime: a missing
				// window is omitted, not recorded as 0.
				continue
			}
			if err := write(filepath.Join(out, "api/v1/endpoints", key, "uptimes", win), v); err != nil {
				return 0, err
			}
		}
	}
	return len(kept), nil
}

// safeKey rejects anything that could escape the output directory.
func safeKey(k string) bool {
	return k != "" && !strings.ContainsAny(k, "/\\") && !strings.Contains(k, "..")
}

func get(ctx context.Context, hc *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

func write(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
