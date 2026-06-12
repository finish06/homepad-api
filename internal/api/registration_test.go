package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// TestRegistrationFailsClosed locks issue #10: HOMEPAD_REGISTRATION must FAIL
// CLOSED. Only "open" (after TrimSpace+ToLower) may open self-registration; any
// other value — invite_only, the hyphenated invite-only, closed, typos — must
// disable it (403). It drives the raw env value the whole way through:
// ParseRegistrationMode -> server -> POST /api/register HTTP status.
//
// The first user always bootstraps as admin regardless of the gate, so each case
// seeds one existing user first; the registration attempt is therefore the
// SECOND user, where the gate actually applies.
func TestRegistrationFailsClosed(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test (needs Postgres)")
	}

	ctx := context.Background()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cases := []struct {
		raw  string
		want int
	}{
		{"open", http.StatusCreated},        // 201 — the one value that opens
		{"invite_only", http.StatusForbidden}, // 403 — underscore spelling
		{"invite-only", http.StatusForbidden}, // 403 — hyphen spelling
		{"closed", http.StatusForbidden},      // 403 — unknown, fail closed
		{"nonsense", http.StatusForbidden},    // 403 — typo, fail closed
		{" OPEN ", http.StatusCreated},        // 201 — whitespace + case normalized
	}

	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			truncateRegistration(t, ctx, dsn)

			// Seed an existing user so the registration attempt is past the
			// first-user admin bootstrap and the gate is actually exercised.
			hash, err := bcrypt.GenerateFromPassword([]byte("seed-pw"), bcrypt.MinCost)
			if err != nil {
				t.Fatalf("hash: %v", err)
			}
			if _, err := store.CreateUser(ctx, "seed@homepad.test", string(hash), "admin"); err != nil {
				t.Fatalf("seed user: %v", err)
			}

			mode, _ := api.ParseRegistrationMode(tc.raw)
			h := api.New(api.Deps{
				Store:        store,
				Sessions:     session.NewManager(),
				Registration: mode,
			})
			srv := httptest.NewServer(h)
			defer srv.Close()

			body, _ := json.Marshal(map[string]string{
				"email":    "newcomer@homepad.test",
				"password": "correct horse battery staple",
			})
			resp, err := http.Post(srv.URL+"/api/register", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST /api/register: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.want {
				t.Errorf("HOMEPAD_REGISTRATION=%q: got status %d, want %d (registration must fail closed for non-\"open\" values)",
					tc.raw, resp.StatusCode, tc.want)
			}
		})
	}
}

func truncateRegistration(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("truncate connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx,
		`TRUNCATE user_collapsed_categories, user_layout, favorites, services, categories, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}
