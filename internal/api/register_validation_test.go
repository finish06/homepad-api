package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// TestRegisterInputValidation locks issue #12: POST /api/register must reject
// malformed emails and too-short passwords with 400 *before* hashing/persisting,
// instead of happily creating accounts like "notanemail" / 1-char passwords.
// Validation runs in decodeCredentials, ahead of the registration gate, so the
// 400 cases short-circuit regardless of registration mode or user count.
func TestRegisterInputValidation(t *testing.T) {
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
		name     string
		email    string
		password string
		want     int
	}{
		{"malformed email", "notanemail", "correct horse battery staple", http.StatusBadRequest},
		{"email no domain", "joe@", "correct horse battery staple", http.StatusBadRequest},
		{"one char password", "joe@homepad.test", "x", http.StatusBadRequest},
		{"seven char password", "joe@homepad.test", "abcdefg", http.StatusBadRequest},
		{"valid email and password", "joe@homepad.test", "abcdefgh", http.StatusCreated},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			truncateRegistration(t, ctx, dsn)

			h := api.New(api.Deps{
				Store:        store,
				Sessions:     session.NewManager(),
				Registration: api.RegistrationOpen,
			})
			srv := httptest.NewServer(h)
			defer srv.Close()

			body, _ := json.Marshal(map[string]string{"email": tc.email, "password": tc.password})
			resp, err := http.Post(srv.URL+"/api/register", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST /api/register: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.want {
				t.Errorf("email=%q password=%q: got status %d, want %d", tc.email, tc.password, resp.StatusCode, tc.want)
			}
		})
	}
}
