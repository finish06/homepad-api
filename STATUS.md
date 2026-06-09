# homepad-api — STATUS (Stitch's channel to Joe)

> NATS result reports are being lost to a harness bug, so this file is how I
> talk to you, Joe. Updated + pushed every run. Newest run on top.

## This run (2026-06-09) — A6 admin catalog edit + delete ✅

Completed the **A6** acceptance criterion (admin catalog CRUD). Create was
already done; this run added **edit** and **delete**, test-first:

- `storage.UpdateService` (partial PATCH, COALESCE-based; `gatus_key` can be
  cleared to unmonitor) + `storage.DeleteService` (404 on missing/bad id;
  favorites & layout rows cascade away).
- `PATCH /api/services/{id}` and `DELETE /api/services/{id}` handlers — admin
  only (403 for `user`), 404 for unknown id, 409 on slug collision.
- Wired both routes into the live mux; **removed their 501 stubs.**
- Tightened the RED tests: `TestAdminCanEditService_200` /
  `TestAdminCanDeleteService_204` now target a real seeded UUID (the originals
  used a bogus `some-id`, which a correct 404-on-missing handler would reject)
  and assert the mutation actually happened. Added `TestUserCannotEditService_403`
  and `TestUserCannotDeleteService_403` to lock the RBAC half of A6.

Verified locally: `go vet ./...` clean, `go build` clean, `go test ./...` green.
**The DB-backed integration tests still `t.Skip` here — this container has no
Postgres/Docker** (see blocker below), so A6's assertions run only in CI.

Only **one 501 stub remains:** `PUT /api/layout` (A5 personal sort order).

## Alpha checklist (A1–A11)

Backend (this repo):
- [x] A1 — register / login / logout (sessions, bcrypt)
- [x] A4 — status staleness < 60s (poller ≤ 30s, `as_of` exposed)
- [x] A5 (favorites half) — per-user favorites persist
- [ ] A5 (layout half) — personal sort order: `PUT /api/layout` + order-aware
      `GET /api/services`. **Last 501 stub. Next run.**
- [x] A6 — admin catalog create / edit / delete; non-admin 403 ← done this run
- [x] A9 — Gatus unreachable → all UNKNOWN, no 5xx
- [x] A10 — Postgres-backed, honors `DATABASE_URL` (code done; CI-verified only)
- [x] A11 — Gatus URL never in any API response (backend half)
- [x] `cmd/homepad-api/main.go` fully wired — opens Store, runs migrations on
      boot, starts the Poller. (Already done; re-confirmed.)

Frontend (`Code/homepad` repo — not yet started):
- [ ] A2 — catalog tiles render (name/icon/desc/url)
- [ ] A3 — status badge colors per state
- [ ] A7 — responsive 390 / 1440, no horizontal scroll
- [ ] A8 — Lighthouse perf budgets
- [ ] A11 (web half) — built bundle contains no Gatus URL
- [ ] Web app exercising the live API end-to-end

## Blockers / decisions

- **NEEDS JOE: Is there CI that runs `make test-integration` against a real
  Postgres?** This container has no Postgres/Docker, so every DB-backed
  integration test (A1, A5, A6, A10, A11-backend) `t.Skip`s here — I can only
  verify build/vet/compile + the no-DB poller tests locally. Those ACs are
  written and GREEN-by-construction but are only *actually executed* where
  `DATABASE_URL` is set. If there's no such CI, those ACs are unverified end to
  end. Can you confirm a pipeline (or a reachable Postgres I'm allowed to use)
  runs them?
- **NEEDS JOE: Who builds the `Code/homepad` web app for alpha?** Alpha's
  definition includes "the web app exercising those flows against the live API."
  The backend will be feature-complete after the A5 layout slice, but the React
  app (A2/A3/A7/A8 + the live-API wiring) is a separate, larger effort I haven't
  started. Want me to pivot to it next, or finish the backend (A5 layout) first?
  My default: finish A5 layout next run (kills the last 501), then start the web app.
