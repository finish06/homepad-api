// Package dbguard holds a single tripwire test that makes a default `go test`
// run fail loudly instead of silently skipping every Postgres-backed
// integration test when DATABASE_URL is unset (issue #13).
//
// Before this guard, `make test` / `go test ./...` printed `ok` while skipping
// all 61 integration tests with no DATABASE_URL — a green that verified nothing.
// This test turns that false-green into a hard, obvious failure on the default
// (non-short) path, while staying out of the way of the unit-only path.
package dbguard

import (
	"os"
	"testing"
)

// TestDatabaseURLRequiredForFullSuite fails when DATABASE_URL is unset on a
// full (non-`-short`) run. The default `make test` target runs without
// `-short`, so a developer with no database now gets a clear failure naming the
// problem rather than a misleading `ok`. The unit-only path (`make test-unit`,
// `go test -short`) skips this guard so it stays runnable with no database.
func TestDatabaseURLRequiredForFullSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: unit-only run, integration tests intentionally skipped")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Fatal("DATABASE_URL is unset: a full `go test ./...` run would SKIP all " +
			"Postgres-backed integration tests and falsely report `ok` (issue #13). " +
			"Run `make test-db` to start an ephemeral Postgres and re-run, or run " +
			"`make test-unit` for unit-only tests.")
	}
}
