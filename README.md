# homepad-api

Backend (Go) for **homepad** — the homelab launcher with live uptime badges.

> **Spec lives in the web repo:** [`Code/homepad/specs/v1-launcher.md`](https://gitea.kube.calebdunn.tech/Code/homepad/src/branch/main/specs/v1-launcher.md)
> Test plan: [`Code/homepad/specs/test-plan-v1.md`](https://gitea.kube.calebdunn.tech/Code/homepad/src/branch/main/specs/test-plan-v1.md)

## Status

🚧 Scaffold only — no business logic yet. All handlers return `501 Not Implemented`.
The test suite is **RED** by design (ADD methodology): tests describe the acceptance criteria, and the GREEN phase drives them to passing one AC at a time.

## Layout

```
cmd/homepad-api/        entry point
internal/api/           HTTP router + handlers (+ integration tests)
internal/gatus/         Gatus client + status poller
internal/session/       in-memory session manager (v1)
internal/storage/       Postgres access + migrations
internal/testsupport/   test harness (httptest server, Gatus stubs)
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
- `.env.example` (the full env contract)
- `specs/v1-launcher.md` § *Deployment contract* (canonical source — image, ports, env, secrets, probes)
