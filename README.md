<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="web/src/brand/horizontal-reverse.svg">
    <img src="web/src/brand/horizontal-color.svg" alt="yuuka" width="240">
  </picture>
</h1>

A self-hosted mail system for catch-all domains. Cloudflare at the edges, Go in the middle,
your own machine in the center.

Cloudflare Email Routing receives, Cloudflare Email Service sends. Everything in
between — raw archive, MIME parsing, classification, search, the web UI and the
agent surface — runs on hardware you control.

> **Status: pre-release.** Receiving and sending work end to end: the Cloudflare
> Email Worker, ingest with redelivery dedupe, MIME parsing, threads and search, a
> WebUI with passkey login and compose, an MCP surface for agents, an outbox with
> crash recovery, per-identity send grants, and an SMTP submission port for other
> services. The rule engine, zones and code extraction (M3) are next, and there are
> no released images yet.

## Design highlights

- **Raw MIME is immutable.** Messages land in object storage before anything parses
  them, so every derived table can be rebuilt from scratch. A parser bug never costs
  you information.
- **`(domain, prefix)` is a dimension, not a mailbox.** There is no "create mailbox"
  step: every address on every catch-all domain is accepted, and unseen prefixes
  register themselves.
- **A prefix registry that detects leaks.** Each prefix records who it was handed to;
  when mail arrives from an unexpected sending domain, that prefix gets flagged.
- **Two zones, handled differently.** Machine mail (verification codes, alerts) is
  aggregated and mined for one-time codes; human correspondence gets full MUA
  semantics — threads, drafts, attachments, multiple signing identities.
- **Agents are first-class, with capabilities enforced at the API layer.** Read freely,
  annotate reversibly, and send according to *whose name is on the mail*. As you, an
  agent can only draft; as its own dedicated address, it can send through automated
  review or within rate limits. Not a prompt-level convention — scopes and per-identity
  grants enforced by the API.
- **Replaceable drivers in both directions.** RFC 5322 bytes are the common currency,
  so one SMTP transport covers Cloudflare, Resend, SES, Gmail and corporate relays,
  and inbound push/pull drivers share a single ingest pipeline.
- **Several principals, one mailbox.** The operator, a named agent and each machine
  (monitoring, schedulers, other self-hosted services) get their own credentials and
  flat scopes — but they all see one message universe. Multi-principal, deliberately
  not multi-tenant: no groups, no OUs, no per-user inboxes. Services send through a
  built-in SMTP submission endpoint, which makes this the single outbound gateway for
  the whole deployment.
- **One inbox over many accounts.** Multiple catch-all domains plus IMAP/Gmail
  accounts, deduplicated into one content layer with per-account sync policy.
- **Offload, not just mirror.** For quota-limited mailboxes, yuuka can become the
  archive of record: once a message is verified in two local copies and covered by
  a backup, the remote copy is deleted to free space. This is the only irreversible
  remote action in the system, policy-driven and never exposed as an API.

## Documentation

The documentation is in Chinese and split by what you are doing;
[`docs/README.md`](docs/README.md) is the entry point.

| You want to | Start at |
|---|---|
| Deploy and run it | [`docs/operate/`](docs/operate/README.md); for Docker Compose with Caddy, Postgres and scheduled backups, [`deploy/README.md`](deploy/README.md) |
| Connect an agent or a service | [`docs/integrate/`](docs/integrate/README.md) — MCP, SMTP submission, REST |
| Work on the code | [`CONTRIBUTING.md`](CONTRIBUTING.md), then the design in [`docs/design/`](docs/design/README.md) |

The design documents explain why the system is shaped the way it is. Their
section numbers (§1–§14) are global across the set and are what code comments
refer to. [`docs/design/roadmap.md`](docs/design/roadmap.md) has the milestones.

## Name

yuuka is the project mascot. The mascot names the whole project rather than one
component of it, so there is no `-mail` suffix — mail is only the first kind of
inbox this is meant to hold.

The mascot also does real work: system messages are attributed to yuuka, while an
agent's summaries and judgements are attributed to the agent itself. That split is
not decoration — it keeps an agent's *guesses* from being read as the system's
*facts*. See [`docs/design/agents.md`](docs/design/agents.md).

## License

Code: [MIT](LICENSE). Logo and wordmark ([`web/src/brand/`](web/src/brand/)): [CC BY 4.0](web/src/brand/LICENSE), by Aerix.
