<p align="center">
  <img src="https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white" alt="Go 1.25">
  <img src="https://img.shields.io/badge/Postgres-backed-336791?logo=postgresql&logoColor=white" alt="Postgres">
  <img src="https://img.shields.io/badge/auth-local%20%2B%20PocketID%20OIDC-6366f1" alt="Local + PocketID OIDC">
  <img src="https://img.shields.io/badge/license-MIT-green" alt="MIT License">
</p>

# homepad-api

> **Looking for Homepad itself?** This is its backend. To run Homepad, use the
> [quickstart](https://github.com/finish06/homepad#quickstart): one compose file
> with published images for amd64 and arm64, and no build. Website:
> [gethomepad.dev](https://gethomepad.dev) · live demo:
> [demo.gethomepad.dev](https://demo.gethomepad.dev).
>
> Image: `ghcr.io/finish06/homepad-api:<version>`, tagged with the same version
> as `ghcr.io/finish06/homepad`.

Backend (Go) for **homepad** — the self-hosted homelab launcher with live uptime
badges. Serves the shared service catalog, per-user favorites & ordering, admin
catalog CRUD, and both auth paths; polls [Gatus](https://github.com/TwiN/gatus)
server-side so the browser never sees the Gatus URL.

Web frontend (React + Vite) lives at [`homepad`](https://github.com/finish06/homepad)
— see its README for the banner, screenshots, and rendered architecture / auth
diagrams.

> **Spec:** [`homepad/specs/v1-launcher.md`](https://github.com/finish06/homepad/blob/main/specs/v1-launcher.md)
> **Test plan:** [`homepad/specs/test-plan-v1.md`](https://github.com/finish06/homepad/blob/main/specs/test-plan-v1.md)

## Status

✅ **Alpha-complete.** Every acceptance criterion this repo owns (A1, A4, A5, A6,
A9, A10, A11-backend) is implemented and green against real Postgres — no `501`
stubs remain. `go test ./...` runs 26 tests green; `go vet ./...` clean.

## What it does

- **Auth** — local register / login / logout (bcrypt, persistent sessions) **and**
  PocketID OIDC (Authorization Code + PKCE, ID-token verified on stdlib crypto,
  account-link by verified email, admin role from the OIDC group). OIDC is fully additive
  and only active when `OIDC_ENABLED=true`.
- **Sessions** — stored in Postgres (migration `0017`), so a restart or deploy
  does not sign anyone out. Only `sha256(token)` is stored, never the token. The
  `homepad_session` cookie is HttpOnly, SameSite=Lax, `Max-Age` 30 days, and
  sliding: the first request a minute or more after the last write extends the
  expiry and re-issues the cookie (so polling writes at most once a minute per
  session). Logout deletes the row; expired rows are swept hourly. Kid sessions
  last 180 days, also sliding.
- **Kid accounts** — a third role, `kid` (migration `0018`), created by an
  admin with just a name: no email, no password, and no way to sign in except
  an admin's "sign in this device as …". A kid sees only the services on its
  allow-list; every catalog read is filtered server-side through
  `storage.VisibleServiceIDs`, and every catalog, library, layout, icon-write,
  admin and settings write is 403. A kid may change its favorites (allowed
  apps only) and its theme. `internal/api/routes_test.go` classifies every
  route and probes each one with a kid session; a new route that is not
  classified there fails the build.
- **Catalog** — one service list for the household; order-aware
  `GET /api/services` merges each user's saved layout. Admin-only create /
  edit / delete (non-admin → 403). No account owns the catalog (migration
  `0021`, 18.0.0), so deleting an account never deletes an app. Slugs and
  section names are unique across the catalog.
- **Overlay sign-in** — each app has `iframeSameOrigin` (migration `0023`,
  18.0.0), the tile editor's "Let this app keep its sign-in". The overlay
  keeps the app's own origin only when it is on. Admins set it on
  `POST`/`PATCH /api/services`; anything but `true` or `false` is a 400. The
  upgrade turns it on for apps already set to Inline overlay. New apps start
  with it off.
- **Migrations** — each `migrations/*.up.sql` runs once, in order, and is
  recorded in `schema_migrations`, all in one transaction under an advisory
  lock. A failed file records nothing, so the next boot tries again. `0021`
  is one way: take a database dump before upgrading to 18.0.0.
- **Per-user state** — favorites and personal sort order, persisted in Postgres.
- **Status poller** — polls Gatus on an interval (≤30s, status `as_of` exposed)
  and serves status per service; Gatus unreachable → all `UNKNOWN`, never a 5xx.
- **A11** — the Gatus URL is never present in any API response.
- **Status widgets** — a few numbers on tiles (Jellyfin, Sonarr, Radarr), a
  clock and the weather, fetched by this server and never by the browser
  (migration `0020`, `internal/widgets`). Every request goes through one guarded
  client: http/https only, no redirects, 5s per request, 1 MiB bodies, and no
  link-local or cloud metadata address (checked on the resolved IP). Keys are
  write-only and sealed with AES-256-GCM under `HOMEPAD_SECRET_KEY` (64 hex
  characters, e.g. `openssl rand -hex 32`); without it, the API refuses to store
  a key and keyless widgets still work. Kids get no widget data. The favicon
  fetcher uses the same client.

### Endpoints

```
POST   /api/register            GET  /api/me
POST   /api/login               POST /api/logout
GET    /api/auth/config         GET  /api/auth/oidc/login        (when OIDC on)
                                GET  /api/auth/oidc/callback      (when OIDC on)
GET    /api/services            POST   /api/services             (admin)
PUT    /api/layout              PATCH  /api/services/{id}         (admin)
                                DELETE /api/services/{id}         (admin)
GET    /api/status              POST   /api/status/refresh        (re-poll Gatus now)
```

Admin Accounts API (all admin-only; `{id}` is always a kid — an adult's id is 404):

```
GET    /api/admin/accounts                      [{id, name, role, lastSeenAt, …}]
POST   /api/admin/kids                          {name, avatarColor?} → 201
PATCH  /api/admin/kids/{id}                     {name?, avatarColor?}
DELETE /api/admin/kids/{id}                     → 204 (sessions, favorites, allow-list go too)
GET    /api/admin/kids/{id}/services            {serviceIds: [...]}
PUT    /api/admin/kids/{id}/services            {serviceIds: [...]} whole-set replace → 204
GET    /api/admin/kids/{id}/sessions            [{id, userAgent, createdAt, lastSeenAt}]
DELETE /api/admin/kids/{id}/sessions/{sid}      → 204 (the device's next request is 401)
POST   /api/admin/kids/{id}/sign-in-here        ends the admin's session, sets the kid's → 204
```

Managing adults (admin-only; `{id}` is always an adult, so a kid's id is 404):

```
PATCH  /api/admin/accounts/{id}                 {role: "admin"|"user"} → 200 account row
DELETE /api/admin/accounts/{id}                 → 204 (sessions, favorites, order, settings go too)
```

A role change signs that person out everywhere. A removal also revokes the
invites they made. Neither touches the shared app list or any kid. An admin
can't act on their own account (409 `{"error":"self"}`), and Homepad always
keeps one admin (409 `{"error":"last_admin"}`).

Backup, restore and Homepage import (all admin-only; SPEC-backup-import):

```
GET    /api/admin/export?people=true|false            the backup file (JSON, schema v2)
POST   /api/admin/restore/preview?mode=merge|replace  body: the backup → Plan
POST   /api/admin/restore?mode=…&plan={planHash}      same body → Result, or 409 + new Plan
POST   /api/admin/import/homepage/preview             {services, bookmarks?, settings?} → Plan
POST   /api/admin/import/homepage?plan={planHash}     same body → Result, or 409 + new Plan
```

A preview changes nothing. An apply re-plans inside one transaction holding an
advisory lock, refuses with 409 when the plan changed since the preview, and
writes everything or nothing. Every write lands in the household's catalog,
whichever admin runs it. A backup holds no ids, password hashes,
sessions, OIDC or env config, or the Gatus URL; its JSON Schema is
[`docs/schema/homepad-backup-v2.json`](docs/schema/homepad-backup-v2.json).
18.0.0 writes `schemaVersion` 2, with the same keys as version 1
([`homepad-backup-v1.json`](docs/schema/homepad-backup-v1.json)), plus an
optional `iframeSameOrigin` on each app; it still restores version 1 files. A
file without `iframeSameOrigin` leaves an existing app's value alone, and a new
app gets `false`. Homepad 17.x refuses a version 2 file.
Homepage `widget`, `widgets` and `providers` values are dropped while parsing
and never stored, logged or returned. `url` and `icon` written by these routes
are always `http(s)` or empty. Limits: a backup up to 16 MB, each Homepage file
up to 1 MB, at most 1,000 apps, 200 sections, 1,000 offers and 100 kids.

Status widgets (SPEC-status-widgets §7):

```
GET    /api/widgets/info                        clock, weather, web search (any session, kids too)
GET    /api/admin/widgets/kinds                 (admin) {secretKeySet, kinds: [...]}
GET    /api/services/{id}/widget                (admin) config; the key is only `hasSecret`
PUT    /api/services/{id}/widget                (admin) {kind, address, secret?, fields, intervalS, …}
DELETE /api/services/{id}/widget                (admin) → 204
POST   /api/services/{id}/widget/test           (admin) unsaved values → {ok, version, fields} | {ok:false, error}
GET    /api/admin/geocode?q=                    (admin) place search for the weather
```

`GET /api/services` carries `widget: {kind, state, asOf, fields}` on a service
with an enabled widget, for adults only. `PATCH /api/admin/settings` takes the
clock, weather and web search settings and echoes them in a `widgets` block;
`GET`/`PATCH /api/me` carry `showWidgets`.

Kid names are 1–40 characters and unique among kids (case-insensitive);
`avatarColor` is one of the frontend accent ids (`blue`, `teal`, `green`,
`yellow`, `orange`, `red`, `pink`, `purple`; default `blue`). A session `id` is
an opaque handle, never the token or its hash.

Invites and sign-up (SPEC-household-basics):

```
GET    /api/auth/config                         {oidcEnabled, registration, setupNeeded}   (public)
POST   /api/admin/invites                       {name, role} → 201 {id, name, role, createdAt, expiresAt, token}
GET    /api/admin/invites                       pending only, newest first (no token)
DELETE /api/admin/invites/{id}                  → 204; 404 when not pending
POST   /api/invites/lookup                      {token} → {name, role, inviterName, expiresAt}   (public)
POST   /api/invites/accept                      {token, email, password, name?} → 201 /api/me body + session   (public)
```

- `HOMEPAD_REGISTRATION` defaults to `invite_only`. The first adult account
  always registers and becomes admin, in every mode, and exactly one wins a
  race (one advisory lock serializes every account insert). After that, an
  admin adds adults with invites; `open` still lets anyone register.
- An invite is for `admin` or `user`, never `kid`, works once and lasts 7
  days. The token is 32 random bytes, base64url, returned once on create;
  only its SHA-256 is stored (migration `0019`). Every dead token (unknown,
  malformed, expired, used, revoked) gets the same 404 at lookup and accept.
  Accept takes its role from the invite; any other field is a 400.
- Lookup and accept share one per-client limit: 30 a minute, burst 10, then
  429 with `Retry-After` (see Sign-in limits below).
- `HOMEPAD_MAX_ACCOUNTS` caps all accounts plus pending invites (unset or `0`:
  no limit; anything else that isn't a whole number stops the API). At the
  cap, kid create, invite create, `open` register and OIDC auto-create answer
  409 `account limit reached (limit N)`. Accepting an invite and the first
  account are never refused.
- Every catalog write acts on the shared catalog for every admin, not just
  the first (#87). Those routes are their own class in `server.go`, and
  `second_admin_test.go` probes each one as a second admin.
- OIDC links or creates an account by email only when the provider's
  `email_verified` claim is true (#89).
- Under `invite_only`, OIDC creates an account only for a new identity in
  `OIDC_ADMIN_GROUP` (admin) or `OIDC_MEMBER_GROUP` (member); an empty
  install only through the admin group. An identity whose verified email
  matches an existing adult always signs in. Under `open`, anyone else is a
  member, as before.
- Every OIDC callback failure, and the start route's own limit, redirect to
  `/#signin-error=<code>`: `email_unverified`, `not_invited`,
  `account_limit`, `sso_failed` or `too_many`. None renders a text page.

Sign-in limits (SPEC-signin-hardening), in memory and per process (a restart
clears them; two replicas each keep their own):

| Route | Per client | Also |
|---|---|---|
| `POST /api/login` | 10 a minute, burst 10 | 10 failures for one email in 15 minutes lock it for 15 minutes, right password or not |
| `POST /api/register` | 5 a minute, burst 5 | |
| `POST /api/invites/lookup`, `/accept` | 30 a minute, burst 10, one bucket | |
| `POST /api/password-resets/lookup`, `/accept` | 30 a minute, burst 10, one bucket | |
| `GET /api/auth/oidc/login` | 20 a minute, burst 10 | over it: 302 to `/#signin-error=too_many` |

- Login, register and the two accept routes run bcrypt. They share a process
  ceiling of 120 a minute, burst 20, which bounds bcrypt work. The two lookup
  routes run no bcrypt and don't spend it. Over a limit: 429, `Retry-After`
  in whole seconds, body `too many attempts`.
- The ceiling trades availability for bounded CPU. It is one bucket for
  everyone. A few clients sending at their full per-client rate can keep it
  empty. While they do, every password sign-in, registration and accept gets
  429. Single sign-on and signed-in sessions keep working.
- The client is the TCP peer. `X-Forwarded-For` counts only when the peer is
  in `HOMEPAD_TRUSTED_PROXIES` (addresses, CIDR ranges or hostnames,
  comma-separated; hostnames resolve at startup and every 5 minutes); then
  the client is the right-most entry that isn't a trusted proxy. `X-Real-IP`
  is never read. Unset, every request through one proxy shares one limit. A
  value that doesn't parse stops the API.
- A hostname that doesn't resolve at startup is tried again every 10 seconds
  until it resolves once. Then it follows the 5-minute refresh. Until then,
  every client behind that proxy shares one limit. The log says so at most
  once every 5 minutes.
- An IPv4 client is its own address. An IPv6 client is its /64, because one
  home or cloud host usually holds a whole /64. An IPv4-mapped IPv6 address
  counts as IPv4.
- The log names the limited client at most once a minute per key, and says
  `login locked for an email` without the email.

The email lock. Ten wrong passwords for one email in 15 minutes lock that
email out of password sign-in for 15 minutes. The lock refuses the right
password too. It answers the same for an email with no account, so it never
shows which emails exist. Anyone who knows an adult's email can lock them
out this way, again and again. The lock touches only password sign-in. To
get back in:

- Wait 15 minutes.
- Sign in with single sign-on, if the account uses it.
- Ask an admin for a reset link. Setting a password with a live link clears
  the lock at once. A dead link clears nothing.
- Change your password on a device that is still signed in. That clears the
  lock too.
- Restarting the API clears every lock.

Password reset links and changing your own password (SPEC-password-reset-link):

```
POST   /api/admin/accounts/{id}/reset-link      (admin) → 201 {id, expiresAt, token}; 404 not an adult; 409 {"error":"self"|"sso_account"}
POST   /api/password-resets/lookup              {token} → {email, name, expiresAt}   (public)
POST   /api/password-resets/accept              {token, password} → 200 /api/me body + session   (public)
POST   /api/me/password                         {currentPassword, newPassword} → 204; 403 {"error":"wrong_password"}; 409 {"error":"sso_account"}
```

- A reset link is for another adult with a password: not yourself (use
  `POST /api/me/password`), not a kid, not an account that only uses single
  sign-on. It works once, for 24 hours. A new link revokes the earlier one.
  The token follows the invite rules: 32 random bytes, base64url, returned
  once, only its SHA-256 stored (migration `0022`), one 404 for every dead
  link. Making a link changes nothing until it is used.
- Accept sets the password, ends every session of that account, and signs
  this device in. Lookup and accept share their own per-client limit.
- Change password ends every other session of the account and keeps this
  one. Five wrong current passwords in 15 minutes lock it for that account
  for 15 minutes (429 with `Retry-After`).
- `GET /api/me` and each `GET /api/admin/accounts` row carry `hasPassword`:
  false for kids and for accounts that only use single sign-on.
- The only admin forgot their password: run
  `docker compose exec api /homepad-api reset-link you@example.com` on the
  server. It prints a one-time `/#reset=…` path and its expiry; put your
  Homepad address in front of it. It exits 1, with the reason on stderr, for
  an email with no adult account or an account that only uses single
  sign-on. The link goes to stdout only and is never logged.

`GET /api/services` carries `responseTimeMs` per service — the latest Gatus
check's response time — **only when there is one**; the key is omitted (never
`0`) for unmonitored services or when Gatus reported no duration.
A service whose latest check **succeeded but took longer than `GATUS_DEGRADED_MS`**
(default `1000`; `0` disables) reads `DEGRADED` — "Slow" on the tile. Gatus has
no degraded state of its own; homepad derives it from the reported duration.
Admins can change the threshold at runtime from the System panel: it is
`statusDegradedMs` on `GET /api/system/config` (the effective value) and
`PATCH /api/admin/settings` (migration `0014`); a saved value wins over the env
var and applies to the next poll with no restart.
`POST /api/status/refresh` re-polls Gatus synchronously: `200 {as_of}` when it
answered, `503 {error, as_of}` when it could not be reached (the last good
snapshot and its `as_of` stand).

`GET /api/me` / `PATCH /api/me` carry `densityPref` (`large|compact|list`, default
`compact`) alongside `themePref`. PATCH takes either or both fields; only the
fields present are validated and written.

`categories.gridWidth` is a **12-column span** — `3` (quarter), `4` (third), `6`
(half) or `12` (full); default `6`. `PATCH /api/categories/{id} {gridWidth}` rejects
anything else with 400. Migration `0013` remapped the old 1–8 tile counts
(1→3, 2→4, 3→6, 4→6, 5→12, 6–8→12) and kept the old value in `grid_width_legacy`.

## Layout

```
cmd/homepad-api/        entry point (opens Store, migrates, starts Poller)
internal/api/           HTTP router + handlers (+ integration tests)
internal/oidc/          PocketID OIDC: discovery, PKCE, ID-token verify, JWKS
internal/gatus/         Gatus client + status poller
internal/session/       session store: Postgres (production) + in-memory (tests)
internal/storage/       Postgres access + migrations
internal/testsupport/   test harness (httptest server, Gatus + IdP stubs)
internal/widgets/       status widgets: guarded client, key sealer, manager, weather
internal/widgets/providers/  Jellyfin, Sonarr, Radarr
migrations/             SQL migrations
```

## Run locally

```bash
cp .env.example .env
make db-up         # docker compose Postgres on :5432
make run           # boots on :8080
```

## Test

```bash
make test          # full suite (Postgres tests skipped if DATABASE_URL unset)
make test-unit     # fast subset
make test-integration  # spins Postgres + runs everything
```

## Deploy

K8s manifests are owned by Joe (homie / SRE bot), not in this repo. This repo ships:

- `Dockerfile` (multi-stage, distroless final image, nonroot)
- `.env.example` (the full env contract, incl. the `OIDC_*` values)
- `specs/v1-launcher.md` § *Deployment contract* (canonical: image, ports, env,
  secrets, probes)
