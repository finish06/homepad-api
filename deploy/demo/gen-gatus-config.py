#!/usr/bin/env python3
"""Regenerate deploy/demo/gatus-endpoints.json from the demo board seed.

One Gatus endpoint per demo tile, checking the tile's public project URL. Group
"demo", name = slug, so Gatus keys them demo_<slug>: the gatus_key that
SeedDemoIfNoUsers writes. These run on the HOMELAB Gatus, not inside the demo:
the demo is Cloud Run with request-scoped CPU and scale-to-zero, where a
background checker never gets to run (homepad-api #76). cmd/demo-status-snapshot
publishes the results, and the demo reads that snapshot as its GATUS_BASE_URL.

Up = reachable and status < 500 (a CDN's bot-wall 403 still means the site is up).
Every 5m, Caleb's decision (homepad-api #79): 52 checks at 2m would be ~37k
requests/day against other people's sites, for freshness a demo board does not
need. The snapshot CronJob also runs every 5m. TestDemoGatusConfigCoversBoard keeps this file and the board in sync.

JSON is valid YAML: paste the array under `endpoints:` in the homelab Gatus config.
"""
import json, re, pathlib
root = pathlib.Path(__file__).resolve().parents[2]
board = json.loads((root / "internal/storage/seed/homepad_demo_board.json").read_text())
eps = []
for e in board:
    slug = re.sub(r"[^a-z0-9]+", "-", e["name"].lower()).strip("-")
    eps.append({"name": slug, "group": "demo", "url": e["url"], "interval": "5m",
                "client": {"timeout": "10s"},
                "conditions": ["[CONNECTED] == true", "[STATUS] < 500"]})
(root / "deploy/demo/gatus-endpoints.json").write_text(json.dumps(eps, indent=2) + "\n")
print(f"wrote {len(eps)} endpoints")
