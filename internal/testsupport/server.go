package testsupport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/gatus"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/oidc"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// NewOIDCServer returns a homepad-api server wired with the given OIDC config,
// backed by a freshly-truncated test Postgres, plus the Store so tests can seed
// and inspect users directly. It is the OIDC-flow counterpart to NewServer:
// no fixtures are seeded, leaving the user table for the test to control.
// Skipped when DATABASE_URL is unset. Cleanup is registered on t.
func NewOIDCServer(t *testing.T, cfg oidc.Config) (*httptest.Server, *storage.Store) {
	t.Helper()

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
	truncate(t, ctx, dsn)

	poller := gatus.NewPoller(gatus.NewClient("http://127.0.0.1:1"), time.Hour)
	h := api.New(api.Deps{
		Store:        store,
		Poller:       poller,
		Sessions:     session.NewManager(),
		Registration: api.RegistrationOpen,
		OIDC:         cfg,
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, store
}

// NewServer returns an httptest.Server running the real homepad-api handler
// backed by a live Postgres (DATABASE_URL). Each call starts from a truncated
// schema and seeds deterministic fixtures (users, sessions, catalog) that the
// integration tests reference by fixed token. The test is skipped when
// DATABASE_URL is unset, mirroring the storage package's integration tests.
// Callers must defer Close().
func NewServer(t *testing.T) *httptest.Server {
	t.Helper()
	// Gatus points at a black hole, so every keyed service resolves UNKNOWN (A9).
	return newServer(t, "http://127.0.0.1:1")
}

// GatusResult is one check in a stubbed Gatus history, oldest-first. A zero
// DurationMs is emitted as no `duration` at all, matching an older Gatus.
type GatusResult struct {
	Success    bool
	DurationMs int64
}

// NewServerWithGatus is NewServer with a LIVE Gatus stub serving the given
// results per endpoint key, for tests that need real statuses / durations
// (responseTimeMs, status refresh). Returns the stub's URL for reference.
func NewServerWithGatus(t *testing.T, endpoints map[string][]GatusResult) (*httptest.Server, string) {
	t.Helper()
	base := time.Date(2026, 9, 13, 1, 0, 0, 0, time.UTC)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/endpoints/statuses" {
			// Uptime windows are best-effort in the poller; a 404 here is omitted.
			http.NotFound(w, r)
			return
		}
		var payload []map[string]any
		for key, results := range endpoints {
			var rs []map[string]any
			for i, res := range results {
				m := map[string]any{"success": res.Success, "timestamp": base.Add(time.Duration(i) * 30 * time.Second)}
				if res.DurationMs > 0 {
					m["duration"] = res.DurationMs * int64(time.Millisecond)
				}
				rs = append(rs, m)
			}
			payload = append(payload, map[string]any{"key": key, "results": rs})
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(stub.Close)
	return newServer(t, stub.URL), stub.URL
}

func newServer(t *testing.T, gatusURL string) *httptest.Server {
	t.Helper()

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
	truncate(t, ctx, dsn)

	admin := seedUser(t, ctx, store, "admin@homepad.test", "stitch-admin-pw", "admin")
	user := seedUser(t, ctx, store, "user@homepad.test", "stitch-user-pw", "user")
	// AC A1's login test signs in as alice without registering first, so seed her.
	seedUser(t, ctx, store, "alice@example.com", "correct horse battery staple", "user")

	sessions := session.NewManager()
	sessions.Bind("admin-session", admin)
	sessions.Bind("non-admin-session", user)
	sessions.Bind("any-user", user)
	sessions.Bind("real-once-impl-exists", user)
	// A5 favorites test signs the same user in twice to prove persistence.
	sessions.Bind("session-one", user)
	sessions.Bind("session-two", user)

	// v9 — services are per-user now. Seed each test user their OWN two
	// monitored entries so /api/services is non-empty for whichever session a
	// test signs in as. Gatus points at a black hole below, so all resolve
	// UNKNOWN (A9).
	for _, owner := range []string{admin, user} {
		seedService(t, ctx, store, owner, "gitea", "Gitea", "core_gitea")
		seedService(t, ctx, store, owner, "grafana", "Grafana", "core_grafana")
	}

	poller := gatus.NewPoller(gatus.NewClient(gatusURL), time.Hour)

	h := api.New(api.Deps{
		Store:        store,
		Poller:       poller,
		Sessions:     sessions,
		Registration: api.RegistrationOpen,
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func truncate(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("truncate connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx,
		`TRUNCATE system_settings, user_collapsed_categories, user_layout, favorites, service_icons, library_apps, services, categories, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func seedUser(t *testing.T, ctx context.Context, store *storage.Store, email, password, role string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	u, err := store.CreateUser(ctx, email, string(hash), role)
	if err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
	return u.ID
}

func seedService(t *testing.T, ctx context.Context, store *storage.Store, userID, slug, name, gatusKey string) {
	t.Helper()
	if _, err := store.CreateService(ctx, userID, storage.Service{
		Slug:     slug,
		Name:     name,
		URL:      "https://" + slug + ".example.com",
		Icon:     slug,
		GatusKey: gatusKey,
	}); err != nil {
		t.Fatalf("seed service %s: %v", slug, err)
	}
}
