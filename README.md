<p align="center">
  <img src="https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white" alt="Go 1.25">
  <img src="https://img.shields.io/badge/Postgres-backed-336791?logo=postgresql&logoColor=white" alt="Postgres">
  <img src="https://img.shields.io/badge/auth-local%20%2B%20PocketID%20OIDC-6366f1" alt="Local + PocketID OIDC">
  <img src="https://img.shields.io/badge/tests-26%20Go%20passing-brightgreen" alt="26 Go tests passing">
  <img src="https://img.shields.io/badge/license-homelab%20%C2%B7%20private-blue" alt="License: homelab / private">
</p>

# homepad-api

Backend (Go) for **homepad** — the self-hosted homelab launcher with live uptime
badges. Serves the shared service catalog, per-user favorites & ordering, admin
catalog CRUD, and both auth paths; polls [Gatus](https://github.com/TwiN/gatus)
server-side so the browser never sees the Gatus URL.

Web frontend (React + Vite) lives at [`Code/homepad`](https://gitea.kube.calebdunn.tech/Code/homepad)
— see its README for the banner, screenshots, and rendered architecture / auth
diagrams.

> **Spec:** [`Code/homepad/specs/v1-launcher.md`](https://gitea.kube.calebdunn.tech/Code/homepad/src/branch/main/specs/v1-launcher.md)
> **Test plan:** [`Code/homepad/specs/test-plan-v1.md`](https://gitea.kube.calebdunn.tech/Code/homepad/src/branch/main/specs/test-plan-v1.md)

## Status

✅ **Alpha-complete.** Every acceptance criterion this repo owns (A1, A4, A5, A6,
A9, A10, A11-backend) is implemented and green against real Postgres — no `501`
stubs remain. `go test ./...` runs 26 tests green; `go vet ./...` clean.

## What it does

- **Auth** — local register / login / logout (bcrypt, in-memory sessions) **and**
  PocketID OIDC (Authorization Code + PKCE, ID-token verified on stdlib crypto,
  account-link by email, admin role from the OIDC group). OIDC is fully additive
  and only active when `OIDC_ENABLED=true`.
- **Catalog** — shared service list; order-aware `GET /api/services` merges each
  user's saved layout. Admin-only create / edit / delete (non-admin → 403).
- **Per-user state** — favorites and personal sort order, persisted in Postgres.
- **Status poller** — polls Gatus on an interval (≤30s, status `as_of` exposed)
  and serves status per service; Gatus unreachable → all `UNKNOWN`, never a 5xx.
- **A11** — the Gatus URL is never present in any API response.

### Endpoints

```
POST   /api/register            GET  /api/me
POST   /api/login               POST /api/logout
GET    /api/auth/config         GET  /api/auth/oidc/login        (when OIDC on)
                                GET  /api/auth/oidc/callback      (when OIDC on)
GET    /api/services            POST   /api/services             (admin)
PUT    /api/layout              PATCH  /api/services/{id}         (admin)
                                DELETE /api/services/{id}         (admin)
```

## Layout

```
cmd/homepad-api/        entry point (opens Store, migrates, starts Poller)
internal/api/           HTTP router + handlers (+ integration tests)
internal/oidc/          PocketID OIDC: discovery, PKCE, ID-token verify, JWKS
internal/gatus/         Gatus client + status poller
internal/session/       in-memory session manager (v1)
internal/storage/       Postgres access + migrations
internal/testsupport/   test harness (httptest server, Gatus + IdP stubs)
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
