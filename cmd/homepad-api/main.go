package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/gatus"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/oidc"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := storage.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("storage: %v", err)
	}
	defer store.Close()

	if err := store.Migrate(ctx); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Println("migrations applied")

	// homepad-db is ephemeral (emptyDir) in prod, so a pod restart wipes the
	// catalog. Self-heal: seed the App Library from the committed catalog when
	// it is empty. Idempotent — a no-op once any offer exists.
	if seeded, err := store.SeedLibraryIfEmpty(ctx); err != nil {
		log.Fatalf("seed library: %v", err)
	} else if seeded > 0 {
		log.Printf("seeded App Library with %d catalog offers (was empty)", seeded)
	}

	gatusClient := gatus.NewClient(os.Getenv("GATUS_BASE_URL"))
	// GATUS_DEGRADED_MS — a succeeded check slower than this reads DEGRADED
	// ("Slow" on the tile). Unset → 1000; 0 disables; junk is logged and ignored.
	if v := os.Getenv("GATUS_DEGRADED_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 0 {
			gatusClient.DegradedAfter = time.Duration(ms) * time.Millisecond
		} else {
			log.Printf("GATUS_DEGRADED_MS=%q is not a non-negative integer; using %s", v, gatus.DefaultDegradedAfter)
		}
	}
	// A threshold saved from the admin System panel wins over the env default.
	if cfg, err := store.SystemSettings(ctx); err == nil && cfg.StatusDegradedMs != nil {
		gatusClient.SetDegradedAfter(time.Duration(*cfg.StatusDegradedMs) * time.Millisecond)
	}
	poller := gatus.NewPoller(gatusClient, 30*time.Second)
	go func() {
		if err := poller.Run(ctx); err != nil && err != context.Canceled {
			log.Printf("poller stopped: %v", err)
		}
	}()

	// Normalize HOMEPAD_REGISTRATION once, here, into a typed mode. Unset is
	// invite_only (SPEC-household-basics AC-001): the first account still
	// registers and becomes admin. Anything other than
	// "open"/"invite_only"/"invite-only" is unrecognized and fails closed
	// (registration disabled) — never fail open on a typo (#10).
	rawReg := os.Getenv("HOMEPAD_REGISTRATION")
	regMode, ok := api.RegistrationFromEnv(rawReg)
	if !ok {
		log.Printf("warning: unrecognized HOMEPAD_REGISTRATION=%q; failing closed (self-registration disabled)", rawReg)
	}

	// HOMEPAD_MAX_ACCOUNTS caps accounts plus pending invites (#88). Junk
	// stops the API: a cap that silently switched off would leave the public
	// demo open.
	maxAccounts, err := api.ParseMaxAccounts(os.Getenv("HOMEPAD_MAX_ACCOUNTS"))
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if maxAccounts > 0 {
		log.Printf("accounts: limit %d (HOMEPAD_MAX_ACCOUNTS)", maxAccounts)
	}

	// #87 — catalog rows a second admin "created" before every admin wrote to
	// the shared catalog are left where they are (Q7); say how many there are.
	if svcs, cats, err := store.NonOwnerAdminCatalogRows(ctx); err != nil {
		log.Printf("catalog: count non-owner admin rows: %v", err)
	} else if svcs+cats > 0 {
		log.Printf("catalog: %d services and %d categories belong to an admin who is not the catalog owner; "+
			"they were never shown and are left in place (#87)", svcs, cats)
	}

	// Sessions persist in Postgres (migration 0017), so a restart or deploy no
	// longer signs anyone out. The sweeper deletes expired rows hourly; main
	// waits for it to stop before the deferred store.Close.
	sessions := session.NewPgStore(store.Pool())
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		session.RunSweeper(ctx, sessions, session.SweepInterval, log.Printf)
	}()
	// Used, revoked and expired invites are kept 30 days for the audit trail,
	// then swept on the same hourly cadence.
	inviteSweepDone := make(chan struct{})
	go func() {
		defer close(inviteSweepDone)
		sweepInvites(ctx, store, session.SweepInterval)
	}()

	h := api.New(api.Deps{
		Store:        store,
		Poller:       poller,
		Sessions:     sessions,
		Registration: regMode,
		OIDC:         oidc.ConfigFromEnv(),
		MaxAccounts:  maxAccounts,
	})

	srv := &http.Server{Addr: ":" + envOr("PORT", "8080"), Handler: h}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Printf("homepad-api listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	// ErrServerClosed only follows ctx cancellation, which also stops the sweeper.
	<-sweeperDone
	<-inviteSweepDone
	log.Println("homepad-api stopped")
}

// sweepInvites deletes stale invites now and then every interval, until ctx
// ends.
func sweepInvites(ctx context.Context, store *storage.Store, every time.Duration) {
	sweep := func() {
		n, err := store.DeleteStaleInvites(ctx, api.InviteRetention)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("invite sweep: %v", err)
			}
			return
		}
		if n > 0 {
			log.Printf("invite sweep: deleted %d old invites", n)
		}
	}
	sweep()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
