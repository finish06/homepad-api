# homepad-api — STATUS (Stitch's channel to Joe)

> NATS result reports are being lost to a harness bug, so this file is how I
> talk to you, Joe. Updated + pushed every run. Newest run on top.

## This run (2026-06-11) — v3 theme-mode BACKEND slice ✅ test-first, all green on the test DB

Joe approved the v3 decisions (Q1 control=header user-menu, Q2 endpoint=`PATCH
/api/me`, Q3 persistence=per-user Postgres + localStorage first-paint cache —
see `Code/homepad/specs/DECISIONS.md`). This run is the **backend slice only**,
test-first per `specs/v3-theme-mode.md`; the web `ThemeProvider` + the
three-segment control is the **next** increment, not this one.

**Branch:** `feat/v3-theme-mode` → base **`main`**. (v2 landed on `main` while I
was working — PRs #3/#4 merged `feat/app-icons` into `main` = `df62360`, so I
rebased onto current `main`; the PR is a clean single-commit v3-only diff with
`0003` correctly on top of `0002`.)

**Migration `0003_theme_pref` (additive, up + down):** one column —
`users.theme_pref TEXT NOT NULL DEFAULT 'system' CHECK (theme_pref IN
('system','light','dark'))`. `DEFAULT 'system'` backfills every existing row to
the intended default (zero data migration); the `CHECK` mirrors the v1/v2
`role`/`variant` pattern so a bad value can never persist. The seeded 39-app
catalog and all other tables are **untouched**. Down is `DROP COLUMN IF EXISTS`
— verified end-to-end against the test DB: up→down drops the column, re-up
re-adds it and existing rows read back `system` (A11).

**Storage (`internal/storage`):** `User` gains `ThemePref`; the user
read/create queries now select/return `theme_pref`; new
`SetThemePref(userID, pref)` updates **only that user's row** (`ErrNotFound` if
the id is unknown).

**API (`internal/api/auth.go` + route):**
- `userView` gains `themePref` (via a small `newUserView` helper), so the stored
  preference rides along on **register, login, and `GET /api/me`** — no extra
  round-trip (A2).
- New **`PATCH /api/me {themePref}`** — session-gated: **401** when
  unauthenticated; **400** on any value other than `system|light|dark` (handler
  validates first; the column `CHECK` is a backstop), leaving the stored value
  unchanged; **200** with the updated `userView` on success. Writes **only the
  caller's row** — there is no path to another user's theme (A5/A6/A7).

**Tests (`internal/api/theme_test.go`, test-first RED→GREEN):** A2 (default
`system` on register + `GET /api/me`), A5 (set `dark` in one session → read back
in a fresh session for the same user), A6 (rejects `neon`/`Dark`/``/`SYSTEM` →
400, stored value unchanged), A7 (no cookie → 401; one user's write leaves
another user's row at `system`). A11 migration round-trip verified directly
against the test DB. **Full suite green** on `homepad-testdb.stitch.svc`,
`go vet ./...` clean, `go build ./...` clean. v1's A1–A11 + v2's icon suite still
pass unchanged.

**Next run:** the web `homepad` v3 slice — `ThemeProvider` (context + live
`matchMedia` following under `system`), the three-segment header control
(optimistic-with-rollback `PATCH`), the anti-flash inline boot script, and
wiring v2's icon precedence to the resolved theme (A1/A3/A4/A8–A10/A12).

## This run (2026-06-10) — v2 app-icons BACKEND slice ✅ test-first, all green on the test DB

Branch `feat/app-icons` off freshly-merged `main`. First v2 increment: the whole
**backend** for custom per-service PNG icons (light/dark), backend-first so the
web edit-mode UI lands next run against real endpoints. Drove it RED→GREEN
against the test Postgres. Decisions Joe signed off (Q1–Q4): fold-in edit mode,
keep 512×512 / 256 KB caps, **reject** oversized (not downscale), **PNG-only**.

**Migration `0002_app_icons` (additive, up + down):** new `service_icons` table
— `(service_id, variant)` PK, `bytes BYTEA`, `byte_size/width/height`, hex
SHA-256 `etag`, `ON DELETE CASCADE` off `services`. `services.icon` text and the
39 seeded rows are **untouched** (zero data migration). Down drops the table for
a clean revert to v1 icon behavior.

**Storage (`internal/storage`):** `Icon` type + `AllIconFlags` (presence map,
never bytes — keeps the list query cheap), `GetIcon`, `PutIcon` (upsert =
create-or-replace), `DeleteIcon` (idempotent). Malformed-UUID / FK-violation →
`ErrNotFound`, matching the existing service-CRUD error mapping.

**4 handlers (`internal/api/icons.go`) + routes:**
- `GET  /api/services/{id}/icon/{variant}` — session-gated; `image/png` + quoted
  `ETag` + `Cache-Control: private, max-age=300`; `If-None-Match` → **304**; 404
  when absent.
- `PUT  …/icon/{variant}` — **admin-only (403)**; raw PNG body; validation order
  **size → magic-byte sniff → dimensions**: >256 KB → **413**, non-PNG (even
  with spoofed `Content-Type: image/png`) → **415**, outside 16–512 px → **422**,
  valid → **204** (upsert).
- `DELETE …/icon/{variant}` — **admin-only**; idempotent **204**.
- bad `{variant}` (not light/dark) → **400** on every verb.

**List response:** `GET /api/services` now carries `iconLight`/`iconDark`
booleans per entry; blob bytes are **never** in the list.

**Tests (`internal/api/icons_test.go`):** new integration coverage for A3, A4,
A5, A6 (incl. boundary cases + bad-variant 400), A10, A11, A12 (cascade verified
by a direct `service_icons` row count), A13. **Full suite green** on the test DB,
`go vet` + `gofmt` clean, `go build ./...` clean. v1's A1–A11 suite still passes
unchanged. CI runs the same `go test` on push.

**Next run:** the web `feat/app-icons` slice — edit-mode toggle (admin-only,
folds in v1 add/edit/delete per Q1), per-tile light/dark upload+remove controls,
theme-aware tile rendering via the precedence chain, and the bundled local
default + `<img> onError` that fixes today's broken-image placeholders.

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

## Coverage review — 2026-06-10 (v1 + v2)

Cross-repo AC + coverage review lives in `Code/homepad`'s
`docs/coverage-v1-v2.md`. Backend-relevant findings:

- **Measured:** `go test ./...` against `homepad-testdb.stitch.svc` = **36 pass,
  66.7% total stmts** (`-coverpkg=./...`). Per-pkg self-cover: `api` 65.7%,
  `gatus` 58.1%, `storage` 11.9%* (*exercised via `api` integration tests).
- 🔴 **Merge-state flag:** the **v2 slice (migration `0002`, icon handlers,
  `iconLight/iconDark` flags) + the OIDC work are NOT on `main`** — `origin/main`
  is `fcef7fa` (v1 only); it all sits on `feat/app-icons` (`382c892`). Needs a PR
  merge before v2 is real on the backend.
- ✅ All v2 icon ACs (A3–A6, A10–A14 server-side) pass **on `feat/app-icons`**.
- 🟡 Untested (none are v1/v2 ACs): OIDC failure branches
  (`ConfigFromEnv`/`Userinfo`/`truthy` 0%, `handleOIDCCallback` 45%),
  `gatus.FetchAll` success-parse 23.8%, `0002_app_icons.down.sql` rollback.
- **Closed during review (test-only, green):** `TestLogoutClearsSession` →
  full A1 round-trip (`session.Destroy` 0%→100%);
  `TestRemoveFavoritePersistsAcrossSessions` added (`DELETE /api/favorites`
  was 0%).
