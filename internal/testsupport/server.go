package testsupport

import (
	"context"
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
		Registration: "open",
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

	// Two monitored catalog entries so /api/services is non-empty. Gatus points
	// at a black hole below, so both resolve UNKNOWN (A9).
	seedService(t, ctx, store, "gitea", "Gitea", "core_gitea")
	seedService(t, ctx, store, "grafana", "Grafana", "core_grafana")

	poller := gatus.NewPoller(gatus.NewClient("http://127.0.0.1:1"), time.Hour)

	h := api.New(api.Deps{
		Store:        store,
		Poller:       poller,
		Sessions:     sessions,
		Registration: "open",
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
		`TRUNCATE user_layout, favorites, services, users RESTART IDENTITY CASCADE`); err != nil {
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

func seedService(t *testing.T, ctx context.Context, store *storage.Store, slug, name, gatusKey string) {
	t.Helper()
	if _, err := store.CreateService(ctx, storage.Service{
		Slug:     slug,
		Name:     name,
		URL:      "https://" + slug + ".example.com",
		Icon:     slug,
		GatusKey: gatusKey,
	}); err != nil {
		t.Fatalf("seed service %s: %v", slug, err)
	}
}
