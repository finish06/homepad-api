package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
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

	// DEMO-ONLY (demo-seed branch): the public demo runs on the same ephemeral
	// DB, so also self-heal the shared demo login + pre-populated board when the
	// users table is empty. No-op the moment any user exists — so it never
	// clobbers a live session and is inert on any deploy that has real users.
	if seeded, err := store.SeedDemoIfNoUsers(ctx); err != nil {
		log.Fatalf("seed demo: %v", err)
	} else if seeded {
		log.Println("seeded demo login + curated board (users table was empty)")
	}

	poller := gatus.NewPoller(gatus.NewClient(os.Getenv("GATUS_BASE_URL")), 30*time.Second)
	go func() {
		if err := poller.Run(ctx); err != nil && err != context.Canceled {
			log.Printf("poller stopped: %v", err)
		}
	}()

	// Normalize HOMEPAD_REGISTRATION once, here, into a typed mode. Anything
	// other than "open"/"invite_only"/"invite-only" is unrecognized and fails
	// closed (registration disabled) — never fail open on a typo (#10).
	rawReg := envOr("HOMEPAD_REGISTRATION", "open")
	regMode, ok := api.ParseRegistrationMode(rawReg)
	if !ok {
		log.Printf("warning: unrecognized HOMEPAD_REGISTRATION=%q; failing closed (self-registration disabled)", rawReg)
	}

	h := api.New(api.Deps{
		Store:        store,
		Poller:       poller,
		Sessions:     session.NewManager(),
		Registration: regMode,
		OIDC:         oidc.ConfigFromEnv(),
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
	log.Println("homepad-api stopped")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
