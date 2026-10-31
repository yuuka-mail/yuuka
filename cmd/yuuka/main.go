// Command yuuka is the yuuka mail system.
//
// Usage:
//
//	yuuka serve              webhook endpoint + periodic reconcile
//	yuuka reconcile [--full] one reconcile pass; --full also scans raw/
//	yuuka version
//
// Configuration comes from the environment:
//
//	YUUKA_DATABASE_URL                  Postgres connection URL (required)
//	YUUKA_R2_ENDPOINT                   R2 S3 endpoint, https://<account id>.r2.cloudflarestorage.com (required)
//	YUUKA_R2_RAW_BUCKET                 raw bucket name (required)
//	YUUKA_R2_RAW_ACCESS_KEY_ID          read-only token for the raw bucket (required)
//	YUUKA_R2_RAW_SECRET_ACCESS_KEY
//	YUUKA_R2_PENDING_BUCKET             pending bucket name (required)
//	YUUKA_R2_PENDING_ACCESS_KEY_ID      read-write token for the pending bucket (required)
//	YUUKA_R2_PENDING_SECRET_ACCESS_KEY
//	YUUKA_DATA_DIR                      directory for the local raw copy (required)
//	YUUKA_WEBHOOK_SECRETS               comma-separated webhook secrets, each >= 32 bytes;
//	                                    several are accepted during rotation (serve only, required)
//	YUUKA_LISTEN                        HTTP listen address (default 127.0.0.1:31480)
//	YUUKA_ACCOUNT                       key of the Cloudflare account in deliveries (default "cloudflare")
//	YUUKA_TELEGRAM_BOT_TOKEN            Telegram bot token (optional; set together with the chat id)
//	YUUKA_TELEGRAM_CHAT_ID              Telegram chat id
//	YUUKA_RECONCILE_INTERVAL            pending/ reconcile period for serve (default 5m)
//
// The webhook is served at POST /ingest/cloudflare. Migrations are applied
// on start.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/ingest"
	"github.com/yuuka-mail/yuuka/internal/localraw"
	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/r2"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// version is set at build time via -ldflags.
var version = "0.0.0-dev"

// fullScanInterval follows §2.1: a weekly raw-vs-database scan.
const fullScanInterval = 7 * 24 * time.Hour

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)
	if err := run(os.Args[1:], log); err != nil {
		fmt.Fprintln(os.Stderr, "yuuka:", err)
		os.Exit(1)
	}
}

func usage() error {
	return errors.New("usage: yuuka serve | yuuka reconcile [--full] | yuuka version")
}

func run(args []string, log *slog.Logger) error {
	if len(args) == 0 {
		return usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "version":
		fmt.Printf("yuuka %s\n", version)
		return nil
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return serve(ctx, log)
	case "reconcile":
		fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
		full := fs.Bool("full", false, "also compare every raw object with raw_objects")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return reconcileOnce(ctx, log, *full)
	}
	return usage()
}

type app struct {
	cfg  config
	pool *pgxpool.Pool
	in   *ingest.Ingester
}

func setup(ctx context.Context, log *slog.Logger, needSecrets bool) (*app, error) {
	cfg, err := loadConfig(os.Getenv, needSecrets)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	db := store.New(pool)
	if err := db.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	var notifier ingest.Notifier
	if cfg.TelegramToken != "" {
		notifier = &notify.Telegram{Token: cfg.TelegramToken, ChatID: cfg.TelegramChat, Client: &http.Client{Timeout: 15 * time.Second}}
	}
	in := ingest.New(ingest.Config{
		Raw:      r2.NewRawBucket(cfg.Raw),
		Pending:  r2.NewPendingBucket(cfg.Pending),
		Local:    localraw.Dir{Root: cfg.DataDir},
		DB:       db,
		Account:  cfg.Account,
		Notifier: notifier,
		Log:      log,
	})
	return &app{cfg: cfg, pool: pool, in: in}, nil
}

func (a *app) close() {
	a.in.Wait()
	a.pool.Close()
}

func reconcileOnce(ctx context.Context, log *slog.Logger, full bool) error {
	a, err := setup(ctx, log, false)
	if err != nil {
		return err
	}
	defer a.close()
	st, err := a.in.Reconcile(ctx)
	log.Info("reconcile", "markers", st.Markers, "ingested", st.Ingested, "waiting", st.Waiting,
		"orphaned", st.Orphaned, "failed", st.Failed)
	if err != nil {
		return err
	}
	failed := st.Failed
	if full {
		sc, err := a.in.FullScan(ctx)
		log.Info("full scan", "raw_objects", sc.RawObjects, "missing", sc.Missing, "recovered", sc.Recovered, "failed", sc.Failed)
		if err != nil {
			return err
		}
		failed += sc.Failed
	}
	if failed > 0 {
		return fmt.Errorf("%d item(s) failed; see log", failed)
	}
	return nil
}

func serve(ctx context.Context, log *slog.Logger) error {
	a, err := setup(ctx, log, true)
	if err != nil {
		return err
	}
	defer a.close()
	// Cancelled on return too, so the background loop stops if the
	// listener fails.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	mux := http.NewServeMux()
	mux.Handle("POST /ingest/cloudflare", &ingest.Handler{Secrets: a.cfg.WebhookSecrets, Ingester: a.in, Log: log})
	srv := &http.Server{
		Addr:              a.cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute, // one ingest may fetch 25 MiB from R2
		IdleTimeout:       2 * time.Minute,
	}

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		background(ctx, log, a.in, a.cfg.ReconcileInterval)
	}()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", a.cfg.Listen, "version", version)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		cancel()
		<-loopDone
		return fmt.Errorf("http: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelShutdown()
	err = srv.Shutdown(shutdownCtx)
	<-loopDone
	return err
}

// background runs reconcile every interval and the full scan at start and
// weekly. Failures are logged; the next tick retries.
func background(ctx context.Context, log *slog.Logger, in *ingest.Ingester, interval time.Duration) {
	var lastFull time.Time
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		st, err := in.Reconcile(ctx)
		if err != nil {
			log.Error("reconcile", "err", err)
		} else if st.Markers > 0 {
			log.Info("reconcile", "markers", st.Markers, "ingested", st.Ingested, "waiting", st.Waiting,
				"orphaned", st.Orphaned, "failed", st.Failed)
		}
		if time.Since(lastFull) >= fullScanInterval {
			sc, err := in.FullScan(ctx)
			if err != nil {
				log.Error("full scan", "err", err)
			} else {
				lastFull = time.Now()
				log.Info("full scan", "raw_objects", sc.RawObjects, "missing", sc.Missing, "recovered", sc.Recovered, "failed", sc.Failed)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
