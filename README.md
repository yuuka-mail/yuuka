# yuuka

A self-hosted mail system for catch-all domains. Cloudflare at the edges, Go in the middle,
your own machine in the center.

Cloudflare Email Routing receives, Cloudflare Email Service sends. Everything in
between — raw archive, MIME parsing, classification, search, the web UI and the
agent surface — runs on hardware you control.

> **Status: design stage.** No working code yet. The design is written up in full
> under [`docs/`](docs/) (in Chinese); [`docs/roadmap.md`](docs/roadmap.md) has the
> milestones.

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

All design documents are in Chinese. The section numbering (§1–§13) is global across
the set; [`docs/README.md`](docs/README.md) is the index. Deployment with Docker
Compose (Caddy, Postgres, scheduled backups) is described in
[`deploy/README.md`](deploy/README.md).

| Document | Contents |
|---|---|
| [prior-art.md](docs/prior-art.md) | Why existing Cloudflare mail projects did not fit |
| [architecture.md](docs/architecture.md) | System architecture, eight design principles, Go stack |
| [data-model.md](docs/data-model.md) | Postgres schema; the content/delivery split |
| [drivers.md](docs/drivers.md) | Inbound and outbound driver contracts, capability matrix |
| [agents.md](docs/agents.md) | The agent surface: anchors, capability tiers, proposals, audit |
| [accounts.md](docs/accounts.md) | Multi-account integration and retention policy |
| [frontend.md](docs/frontend.md) | Frontend stack and rendering paths |
| [principals.md](docs/principals.md) | Principals, credentials, scopes, SMTP submission |
| [roadmap.md](docs/roadmap.md) | Milestones M0–M5 and known risks |
| [naming.md](docs/naming.md) | Name, command surface, repository layout |

## Name

yuuka is the project mascot. The mascot names the whole project rather than one
component of it, so there is no `-mail` suffix — mail is only the first kind of
inbox this is meant to hold.

The mascot also does real work: system messages are attributed to yuuka, while an
agent's summaries and judgements are attributed to the agent itself. That split is
not decoration — it keeps an agent's *guesses* from being read as the system's
*facts*. See [`docs/agents.md`](docs/agents.md).

## License

[MIT](LICENSE)
