package dbguard

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
)

// CheckTestDatabase is the single safety guard protecting every destructive
// integration-test operation in this repo (Migrate, TRUNCATE, DROP DATABASE).
// It returns a non-nil error unless dsn points at a throwaway test database:
//
//   - the DSN host must NOT be the production CNPG cluster
//     (homepad-pg-rw.homepad[.svc…]). This check is independent of the name, so
//     a prod database mislabelled "homepad_test" is still refused.
//   - the database name must end in "_test" or equal "homepad_test".
//
// On 2026-09-13 the integration harness, pointed at the prod DSN from
// homepad-api#59, truncated Caleb's entire Homepad catalog. #67 added this guard
// to internal/testsupport, but internal/storage (and internal/api) reached a
// database WITHOUT it — the gap #72 closes. The guard lives here, in a package
// every destructive test imports, so there is ONE copy that cannot drift. There
// is deliberately NO override env var: a copy-pasteable bypass is exactly how
// the accident happened.
func CheckTestDatabase(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("refusing destructive test setup: cannot parse DATABASE_URL: %w", err)
	}
	host := u.Hostname()
	// Hard-refuse the production cluster host, regardless of db name.
	for _, prod := range []string{"homepad-pg-rw.homepad.svc", "homepad-pg-rw.homepad"} {
		if strings.Contains(host, prod) {
			return fmt.Errorf("refusing destructive test setup: DSN host %q is the production cluster (matches %q); integration tests truncate every table and must never run against prod", host, prod)
		}
	}
	// Only a throwaway test database may be migrated/truncated/dropped.
	name := strings.TrimPrefix(u.Path, "/")
	if name == "homepad_test" || strings.HasSuffix(name, "_test") {
		return nil
	}
	return fmt.Errorf("refusing destructive test setup: database %q is not a test database (name must end in \"_test\" or equal \"homepad_test\") — integration tests truncate every table", name)
}

// RequireTestDatabase is the single accessor every destructive integration test
// uses to obtain DATABASE_URL. It skips the test when DATABASE_URL is unset
// (so the unit-only path stays runnable without Postgres), and FATALS — never
// skips (#67 AC2: a skip is how a dangerous run stays invisible) — when the DSN
// is not a throwaway test database, BEFORE the caller can Open, Migrate, or run
// any SQL. Routing every caller through this one function, rather than trusting
// each test to remember a guard, is the point of #72: the 2026-09-13 incident
// recurred because one package reached the database by a path the guard did not
// cover.
func RequireTestDatabase(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping Postgres-backed integration test (needs Postgres)")
	}
	if err := CheckTestDatabase(dsn); err != nil {
		t.Fatalf("%v", err)
	}
	return dsn
}
