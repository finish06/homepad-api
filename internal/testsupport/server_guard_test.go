package testsupport

import "testing"

// TestCheckTestDatabase covers the safety guard that stops this harness from
// truncating a non-throwaway database.
//
// Context: on 2026-09-13 the integration harness was run with DATABASE_URL
// pointed at the production DSN documented in homepad-api#59 and it truncated
// every table in prod, destroying Caleb's Homepad catalog. checkTestDatabase is
// the guard that must make that impossible.
func TestCheckTestDatabase(t *testing.T) {
	tests := []struct {
		name    string
		dsn     string
		wantErr bool
	}{
		{
			// AC5 reject — the exact production DSN from issue #59.
			name:    "prod cluster DSN (issue #59) is refused",
			dsn:     "postgres://homepad:homepad@homepad-pg-rw.homepad.svc.cluster.local:5432/homepad",
			wantErr: true,
		},
		{
			// AC5 accept — a throwaway _test database on a non-prod host.
			name:    "_test database on non-prod host is accepted",
			dsn:     "postgres://homepad:homepad@localhost:5432/homepad_test?sslmode=disable",
			wantErr: false,
		},
		{
			// AC5 accept — the exact DSN used by CI/docker-compose (host
			// "postgres", db homepad_test) must be allowed so the suite still runs.
			name:    "CI/compose homepad_test DSN is accepted",
			dsn:     "postgres://homepad:homepad@postgres:5432/homepad_test?sslmode=disable",
			wantErr: false,
		},
		{
			// AC3 — the prod host is refused even when the db is named homepad_test.
			name:    "prod host with homepad_test name still refused",
			dsn:     "postgres://homepad:homepad@homepad-pg-rw.homepad.svc.cluster.local:5432/homepad_test",
			wantErr: true,
		},
		{
			// AC3 — the short prod host form is also refused.
			name:    "short prod host form is refused",
			dsn:     "postgres://homepad:homepad@homepad-pg-rw.homepad:5432/anything_test",
			wantErr: true,
		},
		{
			// AC2 — a non-test database on a safe host is refused.
			name:    "non-test database on safe host is refused",
			dsn:     "postgres://homepad:homepad@localhost:5432/homepad",
			wantErr: true,
		},
		{
			// AC2 — any *_test suffix on a safe host is accepted.
			name:    "arbitrary _test suffix on safe host is accepted",
			dsn:     "postgres://u:p@127.0.0.1:5432/ci_run_42_test",
			wantErr: false,
		},
		{
			// Fail closed on an unparseable DSN rather than assume it is safe.
			name:    "unparseable DSN is refused",
			dsn:     "://not a dsn",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkTestDatabase(tc.dsn)
			if tc.wantErr && err == nil {
				t.Fatalf("checkTestDatabase(%q) = nil; want a refusal", tc.dsn)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkTestDatabase(%q) = %v; want acceptance", tc.dsn, err)
			}
		})
	}
}
