.PHONY: build test test-unit test-integration run db-up db-down lint tidy clean

build:
	go build -trimpath -o bin/homepad-api ./cmd/homepad-api

test:
	go test ./... -count=1

# Unit tests only — skip anything that needs DATABASE_URL.
test-unit:
	go test ./... -count=1 -run '^Test' -short

# Full suite assuming a Postgres is reachable via DATABASE_URL.
test-integration: db-up
	DATABASE_URL?=postgres://homepad:homepad@localhost:5432/homepad?sslmode=disable \
		go test ./... -count=1

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
