package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/widgets"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/widgets/providers"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// SPEC-password-reset-link Q5 — the lone admin's way back in. It runs
	// against the same database and exits; the server doesn't start.
	if len(os.Args) > 1 && os.Args[1] == "reset-link" {
		os.Exit(runResetLink(context.Background(), os.Getenv("DATABASE_URL"), os.Args[2:], os.Stdout, os.Stderr))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := storage.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("storage: %v", err)
	}
	defer store.Close()

	migrated, err := store.MigrateReport(ctx)
	if err != nil {
		var stopped *storage.MigrationMessageError
		if errors.As(err, &stopped) {
			log.Printf("migrate: %s stopped the upgrade; nothing was changed", stopped.File)
		}
		log.Fatalf("migrate: %v", err)
	}
	log.Printf("migrations applied (%d new)", len(migrated.Applied))
	// SPEC-household-catalog-owner AC-005 — the first 18.0.0 boot moves the
	// catalog rows no dashboard showed to catalog_archive; say how many, once.
	if line := migrated.ArchiveLine(); line != "" {
		log.Print(line)
	}

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

	// HOMEPAD_TRUSTED_PROXIES (SPEC-signin-hardening §2.2): the proxies whose
	// X-Forwarded-For the sign-in limits believe. Unset trusts none, so
	// behind a proxy every client shares one limit. Junk stops the API, like
	// HOMEPAD_MAX_ACCOUNTS. Hostnames resolve now and every 5 minutes.
	trusted, err := api.ParseTrustedProxies(os.Getenv("HOMEPAD_TRUSTED_PROXIES"))
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if trusted.Size() == 0 {
		log.Printf("sign-in limits: HOMEPAD_TRUSTED_PROXIES is unset, so X-Forwarded-For is ignored; behind a proxy, every client shares one limit")
	} else {
		trusted.Refresh(ctx)
		go trusted.Run(ctx, api.TrustedRefreshEvery)
		log.Printf("sign-in limits: trusting X-Forwarded-For from %d HOMEPAD_TRUSTED_PROXIES entries", trusted.Size())
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

	// Status widgets (SPEC-status-widgets). HOMEPAD_SECRET_KEY seals widget
	// keys at rest; without it, widgets that need no key still work and the
	// API refuses to store one. The value is never logged — not even when it
	// is malformed.
	sealer, err := widgets.ParseSecretKey(os.Getenv(widgets.SecretKeyEnv))
	if err != nil {
		log.Printf("warning: %v", err)
	} else if sealer == nil {
		log.Printf("%s is unset: widget keys can't be saved (clock, weather and search still work)", widgets.SecretKeyEnv)
	}
	outbound := widgets.NewClient()
	widgetKinds := providers.Default()
	widgetManager := widgets.NewManager(widgets.ManagerOptions{
		Source: api.NewWidgetSource(store), Registry: widgetKinds, Client: outbound, Sealer: sealer, Logf: log.Printf,
	})
	weather := widgets.NewWeather(widgets.WeatherOptions{
		Client: outbound, Logf: log.Printf,
		Load: func(ctx context.Context) (widgets.WeatherSettings, error) { return api.LoadWeatherSettings(ctx, store) },
	})
	go func() {
		if err := widgetManager.Run(ctx); err != nil && err != context.Canceled {
			log.Printf("widgets stopped: %v", err)
		}
	}()
	go func() {
		if err := weather.Run(ctx); err != nil && err != context.Canceled {
			log.Printf("weather stopped: %v", err)
		}
	}()

	h := api.New(api.Deps{
		Store:          store,
		Poller:         poller,
		Sessions:       sessions,
		Registration:   regMode,
		OIDC:           oidc.ConfigFromEnv(),
		MaxAccounts:    maxAccounts,
		TrustedProxies: trusted,
		Widgets:        widgetManager,
		Weather:        weather,
		Sealer:         sealer,
		WidgetKinds:    widgetKinds,
		Outbound:       outbound,
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

// sweepInvites deletes stale invites and reset links now and then every
// interval, until ctx ends.
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
		// Reset links follow the same 30-day rule (SPEC-password-reset-link §6).
		n, err = store.DeleteStalePasswordResets(ctx, api.ResetRetention)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("reset link sweep: %v", err)
			}
			return
		}
		if n > 0 {
			log.Printf("reset link sweep: deleted %d old reset links", n)
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

// runResetLink is `homepad-api reset-link <email>`: it opens the database at
// dsn and prints a one-time /#reset= path for that adult (AC-016). The token
// goes to stdout only; the audit line goes to the log, with cli as the actor.
func runResetLink(ctx context.Context, dsn string, args []string, stdout, stderr io.Writer) int {
	if dsn == "" {
		fmt.Fprintln(stderr, "DATABASE_URL is unset. Run this where the API runs: docker compose exec api /homepad-api reset-link <email>")
		return 1
	}
	if len(args) != 1 {
		return api.ResetLinkCommand(ctx, nil, args, stdout, stderr)
	}
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		fmt.Fprintf(stderr, "Couldn't open the database: %v\n", err)
		return 1
	}
	defer store.Close()
	return api.ResetLinkCommand(ctx, store, args, stdout, stderr)
}
