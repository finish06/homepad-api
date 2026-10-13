#!/usr/bin/env python3
"""Regenerate deploy/demo/gatus.yaml from the demo board seed.

Every demo tile gets a real Gatus check against its public project URL, keyed
demo_<slug>, which matches the gatus_key SeedDemoIfNoUsers writes. The checks
count a site as up when it is reachable and answers below 500. A 403 from a
bot-blocking CDN still means the site is reachable, so it isn't reported DOWN.
TestDemoGatusConfigCoversBoard keeps this file and the board in sync.
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
cfg = {"web": {"port": 8082}, "storage": {"type": "memory"}, "endpoints": eps}
hdr = ("# GENERATED from internal/storage/seed/homepad_demo_board.json by\n"
       "# deploy/demo/gen-gatus-config.py. Do not edit by hand; regenerate.\n"
       "# JSON is valid YAML, so Gatus reads this file as-is.\n")
(root / "deploy/demo/gatus.yaml").write_text(hdr + json.dumps(cfg, indent=2) + "\n")
print(f"wrote {len(eps)} endpoints")
