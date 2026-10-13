package storage

import (
	"encoding/json"
	"os"
	"testing"
)

// TestDemoGatusConfigCoversBoard keeps deploy/demo/gatus-endpoints.json in lockstep with
// the demo board: every seeded tile's gatus_key must have a Gatus endpoint, and
// every endpoint must belong to a tile. Otherwise a tile reads UNKNOWN on the
// public demo, or a check runs for nothing. No database needed.
func TestDemoGatusConfigCoversBoard(t *testing.T) {
	var board []demoBoardEntry
	if err := json.Unmarshal(demoBoardJSON, &board); err != nil {
		t.Fatalf("parse board: %v", err)
	}
	raw, err := os.ReadFile("../../deploy/demo/gatus-endpoints.json")
	if err != nil {
		t.Fatalf("read gatus endpoints: %v", err)
	}
	var cfg struct {
		Endpoints []struct {
			Name  string `json:"name"`
			Group string `json:"group"`
			URL   string `json:"url"`
		}
	}
	if err := json.Unmarshal(raw, &cfg.Endpoints); err != nil {
		t.Fatalf("parse gatus config (regenerate with deploy/demo/gen-gatus-config.py): %v", err)
	}
	keys := map[string]string{}
	for _, ep := range cfg.Endpoints {
		keys[ep.Group+"_"+ep.Name] = ep.URL
	}
	seen := map[string]bool{}
	for _, e := range board {
		k := demoGatusKey(demoSlug(e.Name))
		seen[k] = true
		url, ok := keys[k]
		if !ok {
			t.Errorf("board tile %q (key %s) has no Gatus endpoint; regenerate deploy/demo/gatus-endpoints.json", e.Name, k)
			continue
		}
		if url != e.URL {
			t.Errorf("tile %q checks %s but launches %s", e.Name, url, e.URL)
		}
	}
	for k := range keys {
		if !seen[k] {
			t.Errorf("Gatus endpoint %s has no board tile", k)
		}
	}
	if len(board) == 0 || len(cfg.Endpoints) != len(board) {
		t.Errorf("board has %d tiles, gatus config %d endpoints", len(board), len(cfg.Endpoints))
	}
}
