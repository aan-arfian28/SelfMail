// Command selfmail runs the provisioning API and the async worker.
//
//	selfmail                      run API (+ worker unless RUN_WORKER=false)
//	selfmail token create <name>  create an admin API token (printed once)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"selfmail/internal/api"
	"selfmail/internal/cloudflare"
	"selfmail/internal/config"
	"selfmail/internal/db"
	"selfmail/internal/dnscheck"
	"selfmail/internal/secret"
	"selfmail/internal/service"
	"selfmail/internal/stalwart"
	"selfmail/internal/store"
	"selfmail/internal/worker"
	"selfmail/migrations"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		return err
	}
	st := store.New(pool)

	if len(os.Args) >= 4 && os.Args[1] == "token" && os.Args[2] == "create" {
		tok, hash, err := secret.NewToken()
		if err != nil {
			return err
		}
		if err := st.CreateCredential(ctx, os.Args[3], hash); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return fmt.Errorf("a token named %q already exists", os.Args[3])
			}
			return err
		}
		fmt.Println(tok) // shown once; only the SHA-256 hash is stored
		return nil
	}

	svc := &service.Service{
		Cfg: cfg, Store: st, Log: log,
		SW:  stalwart.New(cfg.StalwartURL, cfg.StalwartUser, cfg.StalwartPassword, cfg.StalwartTimeout),
		DNS: &dnscheck.Checker{R: dnscheck.NewPublicResolver(cfg.DNSResolvers), Timeout: cfg.DNSTimeout},
	}
	if cfg.CloudflareToken != "" {
		svc.CF = cloudflare.New(cfg.CloudflareToken, cfg.CloudflareZoneID)
	} else {
		log.Warn("CLOUDFLARE_API_TOKEN not set: DNS records must be created manually (GET /domains/{name}/dns-records)")
	}

	// Push thresholds/limits to Stalwart; best-effort so the API can still start.
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	if err := svc.ApplyStalwartSettings(sctx); err != nil {
		log.Warn("could not apply settings to Stalwart (will not retry automatically)", "err", err)
	}
	cancel()

	if cfg.RunWorker {
		go worker.New(svc, st, cfg, log).Run(ctx)
	}

	srv := &http.Server{
		Addr: cfg.ListenAddr, Handler: api.New(svc, st, cfg, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.ListenAddr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sd, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		return srv.Shutdown(sd)
	}
}
