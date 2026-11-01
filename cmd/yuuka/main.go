// Command yuuka is the yuuka mail system.
//
// Usage:
//
//	yuuka migrate            apply pending database migrations, as the schema owner
//	yuuka serve              webhook endpoint + periodic reconcile + pulling external mailboxes
//	                         (+ WebUI and REST API)
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
//	yuuka token issue --principal <kind>:<name> --scope <s1,s2,...> [--label L] [--ttl D]
//	                         bearer token for an agent or service; printed once
//	yuuka token issue --kind smtp --principal <kind>:<name> [--label L] [--ttl D]
//	                         SMTP submission password (user name: its credential id)
//	yuuka token ls [--principal <kind>:<name>]
//	yuuka token revoke <credential-id>
//	yuuka outbox ls [--state S] [--limit N]
//	                         outgoing mail: state, recipients, attempts, last error
//	yuuka outbox resolve <id> --sent|--resend|--abandon
//	                         decide an unknown send (§4.5): it was delivered, send
//	                         it again, or give up
//	yuuka identity add <local>@<zone>|*@<zone> --owner <kind>:<name> --transport NAME
//	                         [--display-name N] [--signature S] [--plain-strip-markup] [--account A]
//	yuuka identity set <local>@<zone>|*@<zone> --transports PRIMARY[,FALLBACK...]
//	yuuka identity ls [--all]
//	yuuka identity rm <local>@<zone>|*@<zone>
//	                         addresses to send as (§9.3); set replaces the
//	                         transport chain; rm disables
//	yuuka transport add NAME --host H [--port P] [--auth plain|none] [--username U]
//	                         [--password-file PATH|-] --caps C
//	yuuka transport set NAME [--host H] [--port P] [--auth A] [--username U]
//	                         [--password-file PATH|-] [--caps C]
//	yuuka transport ls [--all] | rm NAME | test NAME
//	                         outgoing transports and their sealed passwords (§17);
//	                         add and set need YUUKA_KEK_FILE; test connects,
//	                         says EHLO and AUTH, and sends no mail
//	yuuka secret ls | check | rewrap
//	                         sealed third-party secrets: list, open each without
//	                         printing it, rewrap under the current KEK (§17.3)
//	yuuka grant set --identity A --principal <kind>:<name> [--policy P] [--recipients S] [--allow L]
//	                         [--max-per-hour N] [--max-per-day N] [--max-new-recipients-per-day N]
//	                         [--hold-seconds N] [--suspend|--resume]
//	yuuka grant ls [--identity A] [--principal <kind>:<name>]
//	yuuka grant rm --identity A --principal <kind>:<name>
//	                         who may send as an identity, and how far (§13.5, §8.9)
//	yuuka rule ls [--all] | show <id> | rm <id>
//	yuuka rule add --spec <file>|-|<json> [--name N] [--priority P] [--disabled]
//	yuuka rule set <id> [--spec S] [--name N] [--priority P] [--enable|--disable]
//	yuuka rule apply <id> [--since T] [--dry-run]
//	yuuka rule revert <id> [--since T]
//	                         rules that label, classify, archive and set the
//	                         notification of arriving mail (§8.5); apply and
//	                         revert run one over past mail
//	yuuka channel add|ls|rm|test NAME ...
//	yuuka channel bind NAME | unbind|subscribe|unsubscribe BINDING ...
//	                         notification channels, the chats bound to people,
//	                         and what each receives (§17, §8.4); channel.go
//	yuuka account add|set|ls|test|sync|pause|resume|rm|access NAME ...
//	                         external mailboxes pulled over IMAP, their sources and
//	                         who reads them (§9, §10.1.3); account.go. serve pulls them
//	yuuka triage status      the scorer's configuration, how much mail it has
//	                         scored, and whether it answers a probe (§15)
//	yuuka triage rescore --since T
//	                         score mail received since T again, beside the old scores
//	yuuka triage report [--since T]
//	                         how the latest scores answer, per question
//	yuuka mcp --stdio       read-only MCP server on stdin/stdout, acting as the
//	                         bearer token in YUUKA_MCP_TOKEN (§8.7)
//	yuuka version
//
// Configuration comes from the environment:
//
//	YUUKA_DATABASE_URL                  Postgres connection URL (required, except by migrate when
//	                                    the next one is set); everything but migrations runs as its role
//	YUUKA_DATABASE_OWNER_URL            Postgres URL of the role that owns the schema, used only
//	                                    to apply migrations (optional). `yuuka migrate` connects
//	                                    as it, or as YUUKA_DATABASE_URL when unset. Any other
//	                                    command given it migrates on start too, a convenience for
//	                                    development; deployments give it to migrate alone. See db.go
//	YUUKA_R2_ENDPOINT                   R2 S3 endpoint, https://<account id>.r2.cloudflarestorage.com (required)
//	YUUKA_R2_RAW_BUCKET                 raw bucket name (required)
//	YUUKA_R2_RAW_ACCESS_KEY_ID          object read & write token for the raw bucket; it writes only,
//	                                    create-only, sent copies under sent/ and pulled mail under
//	                                    pull/ (§2.2) (required)
//	YUUKA_R2_RAW_SECRET_ACCESS_KEY
//	YUUKA_R2_PENDING_BUCKET             pending bucket name (required)
//	YUUKA_R2_PENDING_ACCESS_KEY_ID      read-write token for the pending bucket (required)
//	YUUKA_R2_PENDING_SECRET_ACCESS_KEY
//	YUUKA_DATA_DIR                      directory for the local raw copy (required)
//	YUUKA_WEBHOOK_SECRETS               comma-separated webhook secrets, each >= 32 bytes;
//	                                    several are accepted during rotation (serve only, required)
//	YUUKA_LISTEN                        HTTP listen address (default 127.0.0.1:31480)
//	YUUKA_ACCOUNT                       name of the Cloudflare account (accounts.name), created on first
//	                                    ingest (default "cloudflare"). Redelivery dedupe is per account:
//	                                    renaming it starts a new account rather than renaming the old one
//	YUUKA_TRUSTED_AUTHSERV_ID           authserv-id of the account's trusted receiver (default "mx.cloudflare.net")
//	YUUKA_TELEGRAM_*                    deprecated: with YUUKA_KEK_FILE, serve imports them as the
//	                                    channel "telegram" once and ignores them after (§17.7)
//	YUUKA_TELEGRAM_BOT_TOKEN            Telegram bot token (optional; set together with the chat id)
//	YUUKA_TELEGRAM_CHAT_ID              Telegram chat id
//	YUUKA_TELEGRAM_OPERATOR_ID          numeric Telegram user id whose presses of the recall button on
//	                                    held mail count (optional; set together with the next one, and
//	                                    the chat id must then be numeric). serve then long-polls the bot
//	YUUKA_TELEGRAM_OPERATOR             human:<name> a recall from Telegram is recorded as
//	YUUKA_RECONCILE_INTERVAL            pending/ reconcile period for serve (default 5m)
//	YUUKA_WEB_ORIGIN                    WebUI origin as the browser sees it, e.g. https://mail.example.com;
//	                                    enables the WebUI, its login and the REST API (required by admin enroll)
//	YUUKA_WEBAUTHN_RP_ID                WebAuthn relying party id (default: the origin's host)
//	YUUKA_TRUSTED_PROXIES               comma-separated CIDRs of reverse proxies whose X-Forwarded-For
//	                                    names the client for login rate limits and the audit log,
//	                                    e.g. 127.0.0.1/32,::1/128 (default: none; the TCP peer is the
//	                                    client and the header is ignored). See internal/clientip
//	YUUKA_MCP_TOKEN                     bearer token `yuuka mcp --stdio` acts as (required there)
//	YUUKA_KEK_FILE                      file holding the KEK that seals third-party secrets (§17.3);
//	                                    without it they are locked. Read by serve, transport, secret
//	YUUKA_KEK_PREVIOUS_FILES            comma-separated KEK files from before a rotation, unwrap only
//	YUUKA_TRANSPORTS                    deprecated (§17.7): serve imports these transports into the
//	                                    database when YUUKA_KEK_FILE is set; one release later they go.
//	                                    Comma-separated names of the outgoing transports, as
//	                                    identities name them (default: none; serve then sends
//	                                    nothing). Each name NAME has, with NAME
//	                                    uppercased and '.' and '-' turned into '_':
//	YUUKA_TRANSPORT_<NAME>_HOST         SMTP server; implicit TLS only (required)
//	YUUKA_TRANSPORT_<NAME>_PORT         default 465
//	YUUKA_TRANSPORT_<NAME>_USERNAME     AUTH PLAIN user (optional)
//	YUUKA_TRANSPORT_<NAME>_PASSWORD_FILE file holding the password; required with USERNAME.
//	                                    Never the password itself in the environment
//	YUUKA_TRANSPORT_<NAME>_CAPS         what the exit lets us set (required): cloudflare,
//	                                    standard (message-id,return-path), none, or a list of
//	                                    message-id, return-path, dkim, status-query (§10.2)
//	YUUKA_OUTBOX_WORKERS                concurrent send attempts in serve (default 2)
//	YUUKA_OUTBOX_POLL_INTERVAL          how often serve looks for due mail (default 30s)
//	YUUKA_SUBMISSION_LISTEN             SMTP submission port of serve, implicit TLS only, e.g. :465
//	                                    (default: off). See internal/smtpd and §13.6
//	YUUKA_SUBMISSION_TLS_CERT_FILE      PEM certificate chain and key for the port; yuuka then
//	YUUKA_SUBMISSION_TLS_KEY_FILE       terminates TLS, and reloads the files when they change.
//	                                    Without them a proxy terminates TLS
//	YUUKA_SUBMISSION_PROXIES            comma-separated CIDRs of the proxies in front of the port.
//	                                    They must send the PROXY protocol (v1 or v2), and only they
//	                                    may; required without the TLS files. A separate list from
//	                                    YUUKA_TRUSTED_PROXIES (§13.6)
//	YUUKA_SUBMISSION_HOSTNAME           name in the port's greeting (default: the first DNS name on
//	                                    the TLS certificate, else the WebUI's host)
//	YUUKA_TRIAGE_URL                    base URL of a System One scorer (§15.6); serve then scores
//	                                    new mail (default: off). Loopback or private addresses only,
//	                                    unless the next is set
//	YUUKA_TRIAGE_HOSTED                 1: the scorer is a third party's; mail content leaves the
//	                                    deployment. https only, and needs the token file
//	YUUKA_TRIAGE_NAME                   the scorer's name in every score, e.g. laya-ml-1 (required
//	                                    with the URL); change it whenever the model changes
//	YUUKA_TRIAGE_MODEL                  model to ask the backend for (default: its choice)
//	YUUKA_TRIAGE_TOKEN_FILE             file holding the scorer's bearer token (optional)
//	YUUKA_TRIAGE_LOOKBACK               how far back serve scores unscored mail (default 30d)
//	YUUKA_TRIAGE_ACCOUNTS               comma-separated names of the accounts whose mail is scored
//	                                    (default: the Cloudflare accounts; pulled mailboxes only when named)
//	                                    See internal/triage.
//
// `yuuka migrate`, `yuuka prefix`, `yuuka token`, `yuuka outbox`,
// `yuuka identity`, `yuuka grant` and `yuuka rule` only need the database URLs;
// `yuuka transport` and `yuuka secret` those and YUUKA_KEK_FILE to seal or open;
// `yuuka triage status` and `report` those and YUUKA_TRIAGE_*, `rescore`
// serve's configuration;
// `yuuka channel` the database URLs, and YUUKA_KEK_FILE to add a channel,
// test it, or tell a chat it was bound;
// `yuuka admin` the database URLs and YUUKA_WEB_ORIGIN. All but migrate
// run on the host without a login; host.go says how their actions are
// recorded.
//
// The webhook is served at POST /ingest/cloudflare (HMAC, §2.2); the login
// endpoints under /auth/ (see internal/authn); the REST API under /api/v1/
// (see internal/api); MCP over streamable HTTP at /mcp, bearer token only
// (see internal/mcp); the WebUI at / (see web); an unauthenticated
// GET /healthz for a reverse proxy's health check (see health.go). The
// route list in serve says how each authenticates.
//
// `yuuka migrate` changes the schema. Every other command checks on start
// that the database is at the version it was built for and refuses to run
// otherwise; behind, the error says to run `yuuka migrate`. db.go has the
// one exception, for development: given YUUKA_DATABASE_OWNER_URL, they
// migrate first.
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
	"github.com/yuuka-mail/yuuka/internal/imap"
	"github.com/yuuka-mail/yuuka/internal/ingest"
	"github.com/yuuka-mail/yuuka/internal/localraw"
	"github.com/yuuka-mail/yuuka/internal/manage"
	"github.com/yuuka-mail/yuuka/internal/mcp"
	"github.com/yuuka-mail/yuuka/internal/outbox"
	"github.com/yuuka-mail/yuuka/internal/pull"
	"github.com/yuuka-mail/yuuka/internal/r2"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/submit"
	"github.com/yuuka-mail/yuuka/internal/transport"
	"github.com/yuuka-mail/yuuka/internal/triage"
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
	return errors.New("usage: yuuka migrate | yuuka serve | yuuka reconcile [--full] | yuuka reparse [--since T] | yuuka admin enroll [--name NAME] [--ttl 15m] | yuuka prefix ls|claim|unclaim | yuuka token issue|ls|revoke | yuuka outbox ls|resolve | yuuka identity add|set|ls|rm | yuuka transport add|set|ls|rm|test | yuuka secret ls|check|rewrap | yuuka grant set|ls|rm | yuuka rule ls|show|add|set|rm|apply|revert | yuuka channel add|ls|rm|test|bind|unbind|subscribe|unsubscribe | yuuka account add|set|ls|test|sync|pause|resume|rm|access | yuuka triage status|rescore|report | yuuka mcp --stdio | yuuka version")
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
	case "migrate":
		fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		d, err := loadMigrateConfig(os.Getenv)
		if err != nil {
			return err
		}
		return migrate(ctx, d, os.Stdout)
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
	case "token":
		return withHost(ctx, func(h *host) error { return h.tokenCmd(ctx, args[1:]) })
	case "outbox":
		return withHost(ctx, func(h *host) error { return h.outboxCmd(ctx, args[1:]) })
	case "identity":
		return withHost(ctx, func(h *host) error { return h.identityCmd(ctx, args[1:]) })
	case "transport":
		return withHost(ctx, func(h *host) error { return h.transportCmd(ctx, args[1:]) })
	case "secret":
		return withHost(ctx, func(h *host) error { return h.secretCmd(ctx, args[1:]) })
	case "grant":
		return withHost(ctx, func(h *host) error { return h.grantCmd(ctx, args[1:]) })
	case "rule":
		return withHost(ctx, func(h *host) error { return h.ruleCmd(ctx, args[1:]) })
	case "channel":
		return withHost(ctx, func(h *host) error { return h.channelCmd(ctx, args[1:]) })
	case "account":
		return withHost(ctx, func(h *host) error { return h.accountCmd(ctx, args[1:]) })
	case "triage":
		return triageCmd(ctx, log, args[1:])
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
	pool, db, err := openDB(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer pool.Close()
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
	// cards are the review tier's Telegram cards; nil without Telegram.
	cards cardSender
	// mail is told of new mail (NewMail); nil without Telegram.
	mail ingest.Notifier
	// notifyRun polls Telegram (review.go); serve only, nil for nothing.
	notifyRun func(context.Context)
	outbox    *outbox.Service // serve only; see openOutbox
	// submit is the only way mail enters the outbox (§8.9): the SMTP
	// submission port, REST and MCP go through it. serve only.
	submit *submit.Service
	// triage scores new mail (§15); serve only, nil without a scorer.
	// Set before anything ingests.
	triage *triage.Runner
	// sealer seals and opens third-party secrets (§17); serve only, nil
	// without a KEK: the locked state.
	sealer *secret.Sealer
	// channels sends test messages through the channels in the
	// database; serve only, nil while they do not run.
	channels api.ChannelTester
}

// newMessage queues a message the content step created for scoring.
func (a *app) newMessage(id string) {
	if a.triage != nil {
		a.triage.Kick(id)
	}
}

// openOutbox builds the outbox and the submission service in front of
// it. Only serve sends, so only serve needs them. The outbox resolves the
// transports from the database at every claim, opening their passwords
// with sealer (nil: those with a password are locked, §17.3). The
// environment's transports, read here with their password files, stand
// in for placeholders of the same name (§17.7); each such name gets a
// placeholder row if it has none, so identities can name it.
func (a *app) openOutbox(ctx context.Context, log *slog.Logger, sealer *secret.Sealer) error {
	trs, err := openTransports(a.cfg.Outbox.Transports, os.ReadFile)
	if err != nil {
		return err
	}
	names := make([]string, len(a.cfg.Outbox.Transports))
	for i, t := range a.cfg.Outbox.Transports {
		names[i] = t.Name
	}
	if err := a.db.EnsureTransports(ctx, names); err != nil {
		return err
	}
	a.outbox = outbox.New(outbox.Config{
		DB:           a.db,
		Raw:          r2.NewRawBucket(a.cfg.Raw),
		Local:        localraw.Dir{Root: a.cfg.DataDir},
		Sealer:       sealer,
		Transports:   trs,
		IndexSent:    a.in.IndexSent,
		Workers:      a.cfg.Outbox.Workers,
		PollInterval: a.cfg.Outbox.PollInterval,
		Alerter:      a.secretAlerter(),
		Log:          log,
	})
	sc := submit.Config{DB: a.db, Outbox: a.outbox, Log: log}
	if a.cards != nil {
		sc.Notifier = a.cards
	}
	a.submit = submit.New(sc)
	return nil
}

func setup(ctx context.Context, log *slog.Logger, needSecrets bool) (*app, error) {
	cfg, err := loadConfig(os.Getenv, needSecrets)
	if err != nil {
		return nil, err
	}
	pool, db, err := openDB(ctx, cfg.DB)
	if err != nil {
		return nil, err
	}
	a := &app{cfg: cfg, pool: pool, db: db}
	// serve decides after its imports (serve, openNotify).
	if !needSecrets {
		if err := a.openNotify(ctx, log, nil, false); err != nil {
			pool.Close()
			return nil, err
		}
	}
	a.in = ingest.New(ingest.Config{
		Raw:      r2.NewRawBucket(cfg.Raw),
		Pending:  r2.NewPendingBucket(cfg.Pending),
		Local:    localraw.Dir{Root: cfg.DataDir},
		DB:       db,
		Account:  cfg.Account,
		Notifier: a,
		// The Cloudflare account's (§17.8); pull accounts keep theirs in
		// account_sources, read per delivery (§10.1.1).
		TrustedAuthservID: cfg.TrustedAuthservID,
		Log:               log,
		NewMessage:        a.newMessage,
	})
	return a, nil
}

func (a *app) mcp() *mcp.Server {
	cfg := mcp.Config{DB: a.db, Raw: a.in, Log: slog.Default(), Version: version}
	// Only serve has the outbox, so only its MCP offers submit_draft; the
	// stdio form writes drafts for serve or a person to submit.
	if a.submit != nil {
		cfg.Submitter = a.submit
	}
	if a.cfg.Triage.On() {
		p := a.cfg.Triage.Profile()
		cfg.Triage = &p
	}
	return mcp.New(cfg)
}

// rest is the REST API, submitting drafts when this process can (serve).
func (a *app) rest(log *slog.Logger) *api.API {
	r := api.New(a.db, a.in, log)
	if a.submit != nil {
		r.WithSubmitter(a.submit)
	}
	if a.cards != nil {
		r.WithSuspendNotifier(suspendCards{cards: a.cards, log: log})
	}
	if a.alerter != nil {
		r.WithAlerter(a.alerter)
	}
	// What the settings need to seal and use third-party secrets
	// (§17.6); without a KEK they answer that they are locked.
	r.WithSecrets(api.Secrets{
		Sealer: a.sealer,
		GetMe:  getMe,
		Probe: func(ctx context.Context, cfg transport.Config) error {
			smtp, err := transport.NewSMTP(cfg)
			if err != nil {
				return err
			}
			return smtp.Probe(ctx)
		},
		ProbeSource: func(ctx context.Context, s store.Source, password string) (*manage.SourceProbe, error) {
			ctx, cancel := context.WithTimeout(ctx, testTimeout)
			defer cancel()
			return manage.ProbeSource(ctx, imap.Config{}, s, password)
		},
		Channels: a.channels,
	})
	// The scores shown are the configured scorer's; without one, none.
	if a.cfg.Triage.On() {
		r.WithTriage(a.cfg.Triage.Profile(), a.cfg.Triage.Accounts)
	}
	return r
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

	// One Sealer for the import and for sending. Without a KEK both wait
	// and everything else runs; a malformed one stops serve (§17.3).
	sealer, err := loadSealer(os.Getenv)
	if err != nil {
		return err
	}
	a.sealer = sealer
	importEnvTransports(ctx, log, a.db, a.cfg.Outbox.Transports, sealer, os.ReadFile)
	a.importEnvTelegram(ctx, log, sealer)
	// Before the outbox, which hands it the review cards and the
	// security alert of a tampered transport secret.
	if err := a.openNotify(ctx, log, sealer, true); err != nil {
		return err
	}
	if err := a.openOutbox(ctx, log, sealer); err != nil {
		return err
	}
	defer a.submit.Wait()
	rs, err := a.routes(log)
	if err != nil {
		return err
	}
	if a.cfg.Web.Origin == "" {
		log.Info("YUUKA_WEB_ORIGIN not set; WebUI and REST API are off")
	}
	srv := &http.Server{
		Addr:              a.cfg.Listen,
		Handler:           newMux(rs),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute, // one ingest may fetch 25 MiB from R2
		IdleTimeout:       2 * time.Minute,
	}

	// The submission port opens before anything runs in the background,
	// so a port that cannot open stops serve before it does anything.
	// A nil channel never delivers: without the port there is nothing to
	// wait for.
	var subErrc <-chan error
	stopSub := func(context.Context) error { return nil }
	if a.cfg.Submission.Listen != "" {
		if stopSub, subErrc, err = startSubmission(a.cfg.Submission, submitPort{a.submit}, a.db, a.domain(), log); err != nil {
			return err
		}
	}

	// The scorer starts before reconcile, which may fold new mail.
	triageDone := make(chan struct{})
	a.startTriage(ctx, log, triageDone)
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		background(ctx, log, a.in, a.cfg.ReconcileInterval)
	}()
	// The outbox stops claiming on cancel and returns once every attempt
	// in flight is recorded: those short of the commit point are abandoned
	// at once, those past it wait for their final reply (outbox.Run).
	outboxDone := make(chan struct{})
	go func() {
		defer close(outboxDone)
		a.outbox.Run(ctx)
	}()
	// The external mailboxes (§10.1.3). Locked without a KEK, like the
	// transports; the Cloudflare account is not affected.
	pullDone := make(chan struct{})
	go func() {
		defer close(pullDone)
		a.pullManager(log).Run(ctx)
	}()
	// The recall button of held mail and /start, by long polling:
	// nothing listens for Telegram, so nothing is exposed for it (§13.7).
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		if a.notifyRun != nil {
			a.notifyRun(ctx)
		}
	}()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", a.cfg.Listen, "version", version)
		errc <- srv.ListenAndServe()
	}()
	var failed error
	select {
	case err := <-errc:
		failed = fmt.Errorf("http: %w", err)
	case err := <-subErrc:
		failed = fmt.Errorf("submission: %w", err)
	case <-ctx.Done():
	}
	cancel()
	// Both servers drain at once, sharing the grace period: a session in
	// DATA gets to finish and see its 250.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelShutdown()
	subDone := make(chan error, 1)
	go func() { subDone <- stopSub(shutdownCtx) }()
	err = errors.Join(failed, srv.Shutdown(shutdownCtx), <-subDone)
	<-loopDone
	<-triageDone
	<-outboxDone
	<-pullDone
	<-pollDone
	return err
}

// pullManager is the engine that pulls the external mailboxes. Their new mail is
// queued for scoring only when YUUKA_TRIAGE_ACCOUNTS names them.
func (a *app) pullManager(log *slog.Logger) *pull.Manager {
	var scored []string
	if a.cfg.Triage.On() {
		scored = a.cfg.Triage.Accounts
	}
	return pull.New(pull.Config{
		DB: a.db, Raw: r2.NewRawBucket(a.cfg.Raw), Local: localraw.Dir{Root: a.cfg.DataDir},
		Sealer: a.sealer, Content: a.in, TriageAccounts: scored, Log: log,
	})
}

// startTriage runs the scorer when one is configured (§15.6), closing
// done when it stops. Without one, mail is not scored and nothing else
// changes.
func (a *app) startTriage(ctx context.Context, log *slog.Logger, done chan struct{}) {
	tc := a.cfg.Triage
	if !tc.On() {
		log.Info("YUUKA_TRIAGE_URL not set; mail is not scored")
		close(done)
		return
	}
	if tc.Hosted {
		log.Warn("triage: the scorer is hosted; the content of scored mail is sent to a third party", "endpoint", tc.Endpoint)
	}
	a.triage = triage.NewRunner(triage.RunnerConfig{
		Profile: tc.Profile(), Endpoint: tc.EndpointKind(), Scorer: tc.Client(),
		Store: a.db, Raw: a.in, Lookback: tc.Lookback, Accounts: tc.Accounts, Log: log,
	})
	log.Info("triage: scoring mail", "scorer", tc.Name, "endpoint", tc.EndpointKind())
	go func() {
		defer close(done)
		a.triage.Run(ctx)
	}()
}

// domain is the name the submission port greets with: the WebUI's host
// when there is one, which is usually what the services connect to.
func (a *app) domain() string {
	if _, host, err := authn.ParseOrigin(a.cfg.Web.Origin); err == nil {
		return host
	}
	return ""
}

// route is one top-level endpoint of serve and how it authenticates
// (§13.7). The inventory test walks this list, so an endpoint without
// authentication is a listed decision rather than an accident.
type route struct {
	pattern string
	auth    string
	handler http.Handler
}

// How the endpoints authenticate; see route.
const (
	authHMAC     = "hmac"           // the Email Worker's webhook signature (§2.2)
	authBearer   = "bearer"         // agent and service tokens only
	authPasskey  = "passkey"        // the login endpoints, which issue the session
	authSession  = "session|bearer" // the REST API, but for its public contract GET /api/v1/openapi.json
	authNoShell  = "none: static"   // the WebUI page shell; its data comes from /api/
	authNoHealth = "none: health"   // GET /healthz; health.go says why
)

func (a *app) routes(log *slog.Logger) ([]route, error) {
	rs := []route{
		// Regardless of YUUKA_WEB_ORIGIN: a proxy in front of an
		// ingest-only serve needs it as much as one in front of the WebUI.
		{"GET /healthz", authNoHealth, newHealth(a.pool.Ping)},
		// The webhook authenticates by HMAC (§2.2), never by session.
		{"POST /ingest/cloudflare", authHMAC, &ingest.Handler{Secrets: a.cfg.WebhookSecrets, Ingester: a.in, Log: log}},
		// Agents authenticate by bearer token only, never by the WebUI's
		// session cookie (§13.7), so this does not depend on YUUKA_WEB_ORIGIN.
		{"/mcp", authBearer, a.mcp().Handler(authn.NewBearer(a.db, log, nil))},
	}
	if a.cfg.Web.Origin == "" {
		return rs, nil
	}
	svc, err := authn.New(authn.Config{DB: a.db, Origin: a.cfg.Web.Origin, RPID: a.cfg.Web.RPID, Alerter: a.alerter, Proxies: a.cfg.Proxies, Log: log})
	if err != nil {
		return nil, err
	}
	return append(rs,
		route{"/auth/", authPasskey, svc.Handler()},
		route{"/api/", authSession, a.rest(log).Handler(svc)},
		// The page shell needs no session; it fetches /auth/session.
		route{"/", authNoShell, web.Handler()},
	), nil
}

func newMux(rs []route) *http.ServeMux {
	mux := http.NewServeMux()
	for _, rt := range rs {
		mux.Handle(rt.pattern, rt.handler)
	}
	return mux
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
