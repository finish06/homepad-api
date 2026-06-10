# homepad-api — STATUS (Stitch's channel to Joe)

> NATS result reports are being lost to a harness bug, so this file is how I
> talk to you, Joe. Updated + pushed every run. Newest run on top.

## This run (2026-06-10) — README tidy (docs only, no code)

Refreshed this repo's README to match reality: replaced the stale "scaffold /
all handlers 501" status with the alpha-complete summary (A1/A4/A5/A6/A9/A10/
A11-backend green, 26 Go tests), added shields.io badges (Go 1.25, Postgres,
local+PocketID auth, tests, license), the full endpoint list, and the
`internal/oidc` package in the layout. Banner + rendered architecture/auth
mermaid diagrams + screenshots live in the web repo's README, which this one
links to. No Go code touched; suite unchanged.

## This run (2026-06-10) — PocketID / OIDC login (backend) ✅ ADDITIVE, tests green on the test DB

New requirement: log in with PocketID (homelab OIDC), **additive** — local
email+password login is untouched and still works. This run is the foundational
backend slice: the full Authorization-Code-with-PKCE login + callback, account
linking, and admin-group mapping, all proven against a **mocked IdP + the real
test Postgres**. Web button is the next run.

**New `internal/oidc` package (no new deps — stdlib crypto):**
- `ConfigFromEnv` reads every value you wire at deploy: `OIDC_ENABLED`,
  `OIDC_DISCOVERY_URL` (or `OIDC_ISSUER`), `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`,
  `OIDC_REDIRECT_URL`, `OIDC_ADMIN_GROUP`. Nothing hardcoded.
- `Provider`: lazy discovery (cached), `AuthCodeURL` (PKCE S256 + state + nonce),
  `Exchange` (code→tokens with the verifier), `Userinfo` (fallback only).
- `verify.go`: ID-token validation hand-rolled on stdlib — RS256 signature
  against the discovery `jwks_uri`, then `iss` / `aud` / `exp` / `nonce`. JWKS
  cached, refreshed once on unknown `kid` (key rotation).
- `Pending`: in-memory state→{verifier,nonce} store, 10-min TTL, single-use
  (mirrors the existing in-memory session.Manager; single-replica is fine).

**New endpoints (only registered when `OIDC_ENABLED=true`):**
- `GET /api/auth/oidc/login` → 302 to PocketID authorize (PKCE/state/nonce
  stashed server-side by state).
- `GET /api/auth/oidc/callback` → validate state, exchange code, verify ID token,
  resolve user, set the **same `homepad_session` cookie** local login uses, 302
  to `/`. Rest of the app is unchanged.
- `GET /api/auth/config` → `{"oidcEnabled":bool}` (always present, so the web can
  gate the button). When `OIDC_ENABLED=false` the two oidc endpoints are
  unregistered → 404, and homepad is local-only.

**Account linking BY EMAIL:** existing local row with that email → reused as-is
(role preserved, password hash never touched). No row → created; role = `admin`
iff the user's `groups` claim contains `OIDC_ADMIN_GROUP`, else `user`.
OIDC-provisioned rows store a non-bcrypt sentinel in `password_hash`, so they can
never local-login (no schema/migration change — fully additive).

**Tests (test-first, all green on the test DB):** a self-contained mock IdP
(httptest serving discovery + JWKS + authorize + token + userinfo, self-signed
RSA key) drives the real browser round-trip incl. PKCE verification end-to-end:
- admin-from-group creates an admin · regular user when not in the group ·
  link-by-email reuses the existing row (no dup, role preserved) · disabled →
  404 + config reports false · enabled → config reports true.
- `go test ./... -count=1` green; `go vet ./...` clean. Existing suite unchanged.

**NEEDS JOE:** the real **`OIDC_ADMIN_GROUP`** value (PocketID group name) — I
read it from env, so just set it at deploy; tests use a placeholder
`homepad-admins`. Also the real client id/secret/redirect/issuer at deploy time.

_Follow-ups (not blockers): negative-path unit tests for verify (bad sig / wrong
nonce) — happy path is covered by the integration test; web "Log in with
PocketID" button + config gate is next run._

## This run (2026-06-09) — A5 layout slice ✅ + WHOLE SUITE GREEN ON REAL POSTGRES ✅

Two things this run: (1) finished the **A5 layout** slice — the last 501 stub is
gone; (2) thanks to the throwaway test DB you stood up, **every DB-backed test
now actually executes** (no more `t.Skip`). The full backend suite is green.

**A5 layout (test-first, RED → GREEN):**
- Rewrote `TestPersonalSortOrderPersistsAcrossSessions` (was skipped + used bogus
  ids). It now PUTs a *reversed* order of the two real seeded services and proves
  a fresh session reads `GET /api/services` back in that order. Confirmed RED
  (501) before implementing.
- `storage.SetLayout(userID, orderedIDs)` — full-replacement of a user's order in
  one tx (delete-then-insert by position); a bogus/unknown id → `ErrNotFound`.
- `ListServices` is now **order-aware**: `LEFT JOIN user_layout … ORDER BY
  sort_index NULLS LAST, name`. Placed services first in saved order, unplaced
  ones fall back to name order.
- `PUT /api/layout` handler (`{"order":[ids]}` → 204, 404 on unknown id).
  Wired into the live mux; **removed the last 501 stub.**

**Verifying against your test DB (`DATABASE_URL` exported, ANSWER 1):**
Running `go test ./...` with the DB exposed **3 genuine failures** that were
hidden while everything skipped — all now fixed:
1. *Cross-package migration race* — `CREATE EXTENSION IF NOT EXISTS` is not
   concurrency-safe, so the api + storage test binaries migrating the shared DB
   in parallel raced on `pg_extension_name_index` (23505). Fixed: `Migrate` now
   runs inside one tx behind a `pg_advisory_xact_lock`, so concurrent migrators
   serialize (also makes multi-replica boot safe). Plain `go test ./...` (parallel
   packages) is now stable — ran 3× uncached, green each time.
2. *`TestRegisterCreatesUser`* registered `alice@example.com`, but the fixture
   seeds alice (the login test needs her) → 409. Test now registers a non-seeded
   email.
3. *`TestAdminCanCreateService_201`* created slug `gitea`, already seeded → 409.
   Test now creates a non-seeded slug (`jellyfin`).

**Verified results — `DATABASE_URL` set, `go test -count=1 ./...`:**
- **21 tests PASS, 0 SKIP, 0 FAIL.** `go vet ./...` clean, `go build ./...` clean.
- Tests that NOW ACTUALLY RAN (were `t.Skip` in every prior run):
  - api: TestRegisterCreatesUser, TestLoginSetsSessionCookie, TestMeUnauthorized,
    TestMeAuthorized, TestLogoutClearsSession, TestUserCannotCreateService_403,
    TestAdminCanCreateService_201, TestAdminCanEditService_200,
    TestUserCannotEditService_403, TestAdminCanDeleteService_204,
    TestUserCannotDeleteService_403, TestMarkFavoritePersistsAcrossSessions,
    **TestPersonalSortOrderPersistsAcrossSessions** (A5, new), and the gatus
    black-hole + A11 no-leak suites.
  - storage: TestStorageBootsWithDatabaseURL, TestMigrationsApplyCleanlyToFreshDB.
- Tests skipped: **none.** (The poller tests never needed a DB; they pass too.)

So A1, A5, A6, A9, A10, A11-backend are now **executed and green against real
Postgres**, not green-by-construction. The backend is feature-complete for alpha.

## Previous run (2026-06-09) — A6 admin catalog edit + delete ✅

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

Backend (this repo) — all now EXECUTED & GREEN on real Postgres:
- [x] A1 — register / login / logout (sessions, bcrypt)
- [x] A4 — status staleness < 60s (poller ≤ 30s, `as_of` exposed)
- [x] A5 (favorites half) — per-user favorites persist
- [x] A5 (layout half) — personal sort order: `PUT /api/layout` + order-aware
      `GET /api/services` ← done this run; **last 501 stub removed**
- [x] A6 — admin catalog create / edit / delete; non-admin 403
- [x] A9 — Gatus unreachable → all UNKNOWN, no 5xx
- [x] A10 — Postgres-backed, honors `DATABASE_URL` (verified against test DB)
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

- **RESOLVED (ANSWER 1): DB-backed tests now verified.** You stood up
  `homepad-testdb.stitch.svc.cluster.local:5432`. I exported `DATABASE_URL` and
  ran the full suite against it — all integration tests execute and pass (see
  this run's verified results above). No CI built by me, as instructed; you're
  adding the durable Gitea Actions + Postgres-service workflow separately. Note
  for that workflow: run `go test ./...` (parallel packages) is fine now that
  `Migrate` is advisory-locked — no `-p 1` needed.
- **RESOLVED (ANSWER 2): ordering approved.** Backend (A5 layout) finished this
  run. **Next run I pivot to the `Code/homepad` web app** (A2/A3/A7/A8 + live-API
  wiring). I own app code; you own the K8s deploy manifests.

No open blockers. Backend is alpha-complete and green.

## Merge record — 2026-06-10

- PR #1 `feat/catalog-vertical-slice` → `main` **merged** via real merge commit `04eb7d2` (parents `17725ebe06` + `69ac38a842`). CI run #561 (Backend vet/build/tests, pull_request) concluded **success**; mergeable was true. Source branch deleted. — Stitch
