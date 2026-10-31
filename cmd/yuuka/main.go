// Command yuuka is the yuuka mail system.
//
// Usage:
//
//	yuuka serve              webhook endpoint + periodic reconcile (+ WebUI and REST API)
//	yuuka reconcile [--full] one reconcile pass; --full also scans raw/
//	yuuka reparse [--since T] rebuild derived content of messages received
//	                         at or after T (RFC 3339 or YYYY-MM-DD; default all)
//	yuuka admin enroll [--name NAME] [--ttl 15m]
//	                         one-time passkey enrollment link + new recovery codes
//	yuuka prefix ls [--state claimed|discovered] [--zone Z]
//	                         the prefix registry with first/last seen and deliveries
//	yuuka prefix claim <local>@<zone>
//	yuuka prefix unclaim <local>@<zone>
//	                         set a prefix claimed or back to discovered (§3.3)
//	yuuka mcp --stdio        read-only MCP server on stdin/stdout, acting as the
//	                         bearer token in YUUKA_MCP_TOKEN (§8.7)
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
//	YUUKA_TRUSTED_AUTHSERV_ID           authserv-id of the account's trusted receiver (default "mx.cloudflare.net")
//	YUUKA_TELEGRAM_BOT_TOKEN            Telegram bot token (optional; set together with the chat id)
//	YUUKA_TELEGRAM_CHAT_ID              Telegram chat id
//	YUUKA_RECONCILE_INTERVAL            pending/ reconcile period for serve (default 5m)
//	YUUKA_WEB_ORIGIN                    WebUI origin as the browser sees it, e.g. https://mail.example.com;
//	                                    enables the WebUI, its login and the REST API (required by admin enroll)
//	YUUKA_WEBAUTHN_RP_ID                WebAuthn relying party id (default: the origin's host)
//	YUUKA_MCP_TOKEN                     bearer token `yuuka mcp --stdio` acts as (required there)
//
// `yuuka admin` only needs YUUKA_DATABASE_URL and YUUKA_WEB_ORIGIN;
// `yuuka prefix` only YUUKA_DATABASE_URL. They run on the host
// without a login; host.go says how their actions are recorded.
//
// The webhook is served at POST /ingest/cloudflare (HMAC, §2.2); the login
// endpoints under /auth/ (see internal/authn); the REST API under /api/v1/
// (see internal/api); MCP over streamable HTTP at /mcp, bearer token only
// (see internal/mcp); the WebUI at / (see web). Migrations are applied on
// start.
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

	"github.com/yuuka-mail/yuuka/internal/api"
	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/ingest"
	"github.com/yuuka-mail/yuuka/internal/localraw"
	"github.com/yuuka-mail/yuuka/internal/mcp"
	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/r2"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/web"
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
	return errors.New("usage: yuuka serve | yuuka reconcile [--full] | yuuka reparse [--since T] | yuuka admin enroll [--name NAME] [--ttl 15m] | yuuka prefix ls|claim|unclaim | yuuka mcp --stdio | yuuka version")
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
	case "reparse":
		fs := flag.NewFlagSet("reparse", flag.ContinueOnError)
		since := fs.String("since", "", "only messages received at or after this time (RFC 3339 or YYYY-MM-DD)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		t, err := parseSince(*since)
		if err != nil {
			return err
		}
		return reparse(ctx, log, t)
	case "mcp":
		fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
		stdio := fs.Bool("stdio", false, "serve MCP on stdin/stdout")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if !*stdio {
			return errors.New("usage: yuuka mcp --stdio (the HTTP form is served by yuuka serve at /mcp)")
		}
		return mcpStdio(ctx, log)
	case "admin":
		if len(args) < 2 || args[1] != "enroll" {
			return usage()
		}
		fs := flag.NewFlagSet("admin enroll", flag.ContinueOnError)
		name := fs.String("name", "", "human principal to enroll (default: the only one, or a new human:operator)")
		ttl := fs.Duration("ttl", authn.DefaultEnrollTTL, "how long the enrollment link stays valid")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		return enroll(ctx, log, *name, *ttl)
	case "prefix":
		return withHost(ctx, func(h *host) error { return h.prefixCmd(ctx, args[1:]) })
	}
	return usage()
}

// enroll prints a one-time passkey enrollment link and a fresh set of
// recovery codes for a human principal (§13.7). Being able to run this on
// the host is the highest authority there is, so it needs no login.
func enroll(ctx context.Context, log *slog.Logger, name string, ttl time.Duration) error {
	cfg, err := loadAdminConfig(os.Getenv)
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()
	db := store.New(pool)
	if err := db.Migrate(ctx); err != nil {
		return err
	}
	svc, err := authn.New(authn.Config{DB: db, Origin: cfg.Web.Origin, RPID: cfg.Web.RPID, Log: log})
	if err != nil {
		return err
	}
	e, err := svc.Enroll(ctx, name, ttl)
	if err != nil {
		return err
	}
	fmt.Printf("Passkey enrollment link for human:%s\n", e.Principal.Name)
	fmt.Printf("Single use, valid until %s:\n\n  %s\n\n", e.ExpiresAt.UTC().Format(time.RFC3339), e.URL)
	fmt.Println("Recovery codes. Each works once and can only start a new passkey")
	fmt.Println("registration, never a login. Store them offline; they are not shown")
	fmt.Println("again, and every earlier code is now void:")
	fmt.Println()
	for _, c := range e.RecoveryCodes {
		fmt.Printf("  %s\n", c)
	}
	return nil
}

type app struct {
	cfg     config
	pool    *pgxpool.Pool
	db      *store.DB
	in      *ingest.Ingester
	alerter authn.Alerter // nil without Telegram
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
	var alerter authn.Alerter
	if cfg.TelegramToken != "" {
		tg := &notify.Telegram{Token: cfg.TelegramToken, ChatID: cfg.TelegramChat, Client: &http.Client{Timeout: 15 * time.Second}}
		notifier, alerter = tg, tg
	}
	in := ingest.New(ingest.Config{
		Raw:      r2.NewRawBucket(cfg.Raw),
		Pending:  r2.NewPendingBucket(cfg.Pending),
		Local:    localraw.Dir{Root: cfg.DataDir},
		DB:       db,
		Account:  cfg.Account,
		Notifier: notifier,
		// Each process has one account until accounts exist (§10.1.1).
		TrustedAuthservID: cfg.TrustedAuthservID,
		Log:               log,
	})
	return &app{cfg: cfg, pool: pool, db: db, in: in, alerter: alerter}, nil
}

func (a *app) mcp() *mcp.Server {
	return mcp.New(mcp.Config{DB: a.db, Raw: a.in, Log: slog.Default(), Version: version})
}

// mcpStdio serves MCP to the client that started this process. The
// process can reach the database, but that does not make whoever runs it
// a principal: tool calls act as the bearer token in YUUKA_MCP_TOKEN, with
// its scopes and in its name in the audit log, exactly as over HTTP.
func mcpStdio(ctx context.Context, log *slog.Logger) error {
	token := os.Getenv("YUUKA_MCP_TOKEN")
	if token == "" {
		return errors.New("YUUKA_MCP_TOKEN is required: tool calls act as that bearer token's principal")
	}
	a, err := setup(ctx, log, false)
	if err != nil {
		return err
	}
	defer a.close()
	return a.mcp().ServeStdio(ctx, authn.NewBearer(a.db, log, nil), token)
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
	bl, err := a.in.ProcessBacklog(ctx)
	log.Info("content backlog", "processed", bl.Processed, "failed", bl.Failed)
	if err != nil {
		return err
	}
	failed += bl.Failed
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

// parseSince reads --since: empty is the beginning of time.
func parseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since %q: want RFC 3339 or YYYY-MM-DD", s)
	}
	return t, nil
}

func reparse(ctx context.Context, log *slog.Logger, since time.Time) error {
	a, err := setup(ctx, log, false)
	if err != nil {
		return err
	}
	defer a.close()
	st, err := a.in.Reparse(ctx, since)
	log.Info("reparse", "backlog_processed", st.Backlog.Processed, "backlog_failed", st.Backlog.Failed,
		"messages", st.Messages, "changed", st.Changed, "failed", st.Failed,
		"threads", st.Threads.Threads, "moved", st.Threads.Moved)
	if err != nil {
		return err
	}
	if n := st.Failed + st.Backlog.Failed; n > 0 {
		return fmt.Errorf("%d item(s) failed; see log", n)
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
	// The webhook authenticates by HMAC (§2.2), never by session.
	mux.Handle("POST /ingest/cloudflare", &ingest.Handler{Secrets: a.cfg.WebhookSecrets, Ingester: a.in, Log: log})
	// Agents authenticate by bearer token only, never by the WebUI's
	// session cookie (§13.7), so this does not depend on YUUKA_WEB_ORIGIN.
	mux.Handle("/mcp", a.mcp().Handler(authn.NewBearer(a.db, log, nil)))
	if a.cfg.Web.Origin != "" {
		svc, err := authn.New(authn.Config{DB: a.db, Origin: a.cfg.Web.Origin, RPID: a.cfg.Web.RPID, Alerter: a.alerter, Log: log})
		if err != nil {
			return err
		}
		mux.Handle("/auth/", svc.Handler())
		mux.Handle("/api/", api.New(a.db, a.in, log).Handler(svc))
		// The page shell needs no session; it fetches /auth/session.
		mux.Handle("/", web.Handler())
	} else {
		log.Info("YUUKA_WEB_ORIGIN not set; WebUI and REST API are off")
	}
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
		if bl, err := in.ProcessBacklog(ctx); err != nil {
			log.Error("content backlog", "err", err)
		} else if bl.Processed+bl.Failed > 0 {
			log.Info("content backlog", "processed", bl.Processed, "failed", bl.Failed)
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
