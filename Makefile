.PHONY: build test test-unit test-db test-integration run db-up db-down lint tidy clean

build:
	go build -trimpath -o bin/homepad-api ./cmd/homepad-api

# Full suite. Refuses to run without DATABASE_URL: a bare `go test ./...` would
# SKIP every integration test and falsely report `ok` (#13). Fail loud instead.
test:
	@if [ -z "$$DATABASE_URL" ]; then \
		echo "ERROR: DATABASE_URL is unset — refusing to run (#13)."; \
		echo "A bare 'go test ./...' SKIPs every integration test and falsely prints 'ok'."; \
		echo "  - Full suite on an ephemeral Postgres:  make test-db"; \
		echo "  - Unit tests only (no database):        make test-unit"; \
		exit 1; \
	fi
	go test ./... -count=1 -p 1

# Unit tests only — skip anything that needs DATABASE_URL.
test-unit:
	go test ./... -count=1 -run '^Test' -short

# Spin an ephemeral Postgres, run the FULL suite against it, then tear it down
# (volume removed). The way to actually exercise the integration tests locally.
test-db:
	docker compose up -d postgres
	@echo "waiting for postgres to become healthy..."
	@until [ "$$(docker inspect -f '{{.State.Health.Status}}' $$(docker compose ps -q postgres) 2>/dev/null)" = "healthy" ]; do sleep 1; done
	-DATABASE_URL=postgres://homepad:homepad@localhost:5432/homepad?sslmode=disable go test ./... -count=1 -p 1
	docker compose down -v

# Full suite assuming a Postgres is reachable via DATABASE_URL.
test-integration: db-up
	DATABASE_URL?=postgres://homepad:homepad@localhost:5432/homepad?sslmode=disable \
		go test ./... -count=1 -p 1

run:
	go run ./cmd/homepad-api

db-up:
	docker compose up -d postgres

db-down:
	docker compose down

lint:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf bin
