package storage_test

import (
	"context"
	"os"
	"testing"
	"time"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// DEMO-ONLY (demo-seed branch). Guards the crash this branch's 2026-09-27 rebase
// onto main uncovered, and which would have taken the public demo down
// permanently rather than intermittently.
//
// Migration 0016 adds users.show_uptime_display NOT NULL and then DROPs its
// DEFAULT. SeedDemoIfNoUsers predates the column by two months and omitted it,
// so the INSERT was a not-null violation:
//
//   ERROR: null value in column "show_uptime_display" of relation "users"
//   violates not-null constraint (SQLSTATE 23502)
//
// The seeder is log.Fatalf on error, and the demo's Postgres is an ephemeral
// sidecar wiped on every scale-to-zero — so it boots at migration zero every
// cold start and would have died every time.
//
// 0016's own comment reasoned that "CreateUser always supplies it". True of the
// app path (storage.go:185) and false of this seeder — the one INSERT INTO users
// that is not CreateUser. Verified by reverting the fix: this test fails with the
// 23502 above, and passes with it.
//
// Also records Migrate's cost on an empty DB, because that runs inside a cold
// start a visitor is already waiting ~15s through.
func TestDemoSeedOnEmptyDB(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	start := time.Now()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Logf("MIGRATE on empty DB: %s", time.Since(start).Round(time.Millisecond))

	s2 := time.Now()
	seeded, err := st.SeedDemoIfNoUsers(ctx)
	if err != nil {
		t.Fatalf("SEED FAILED (this is the boot crash): %v", err)
	}
	t.Logf("SEED: seeded=%v in %s", seeded, time.Since(s2).Round(time.Millisecond))

	// Idempotence: a second call must be a no-op, since Migrate+seed run every boot.
	again, err := st.SeedDemoIfNoUsers(ctx)
	if err != nil {
		t.Fatalf("second seed errored: %v", err)
	}
	if again {
		t.Fatalf("second seed re-seeded — not idempotent")
	}
	t.Logf("second call: no-op as expected")
}
