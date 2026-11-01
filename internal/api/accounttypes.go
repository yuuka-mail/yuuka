package api

import "time"

// Accounts (§9.5, §13.8): what any principal may learn about the
// accounts it reads, and the pull accounts' sources as only a person's
// session sees and changes them. A source's password is write-only: what
// is shown of it is its SecretStatus.

// AccountKind is an account's kind: cf_catchall, the Cloudflare Email
// Routing catch-all mail is pushed through; imap, a mailbox yuuka pulls.
type AccountKind string

func (AccountKind) Enum() []string { return []string{"cf_catchall", "imap"} }

func accountKind(k string) AccountKind { return AccountKind(k) }

// ReadableAccount is an account the viewer reads: no host, user name or
// sync state, which are a person's configuration (§9.4).
type ReadableAccount struct {
	Name string      `json:"name"`
	Kind AccountKind `json:"kind"`
	// Address is a pull account's mailbox; absent for a catch-all, which
	// has many.
	Address string `json:"address,omitempty"`
}

// AccountList is GET /api/v1/accounts: the accounts the viewer may read,
// by name. search's account: takes these names.
type AccountList struct {
	Accounts []ReadableAccount `json:"accounts"`
}

// SourceState is whether a pull account's source is pulled: enabled;
// paused by a person; login_refused, paused because the server refused
// the password (resuming tries it again); removed, its password erased
// and its mail kept.
type SourceState string

func (SourceState) Enum() []string { return []string{"enabled", "paused", "login_refused", "removed"} }

// SourcePreset is a source's preset: gmail, whose host, folders and
// daily budget are Gmail's (§10.1.3).
type SourcePreset string

func (SourcePreset) Enum() []string { return []string{"gmail"} }

// AccessEntry is one principal's own setting for an account: it may or
// may not read it, whatever the account's default.
type AccessEntry struct {
	// Principal is kind:name.
	Principal string `json:"principal"`
	CanRead   bool   `json:"can_read"`
}

// SourceFolder is how far one remote folder has been read. UIDs are the
// server's; every one between Low and High is fetched or failed.
type SourceFolder struct {
	Name string `json:"name"`
	// SpecialUse is its RFC 6154 attribute, e.g. \All; absent if none.
	SpecialUse  string `json:"special_use,omitempty"`
	UIDValidity uint32 `json:"uidvalidity"`
	// High is the newest UID fetched; catching up is done when it is
	// UIDNext - 1.
	High uint32 `json:"high"`
	// Low is how far back backfill got: 0 not started, 1 done.
	Low uint32 `json:"low"`
	// UIDNext and Messages are what the server said last; 0 before.
	UIDNext  uint32 `json:"uid_next"`
	Messages uint32 `json:"messages"`
	// Rematching: the server changed UIDVALIDITY, and what was pulled is
	// being matched to the new UIDs.
	Rematching    bool       `json:"rematching"`
	DraftsSkipped int64      `json:"drafts_skipped"`
	LastRoundAt   *time.Time `json:"last_round_at,omitempty"`
}

// PullFailureView is a message that could not be fetched yet. It is
// tried again on a sync that asks for it.
type PullFailureView struct {
	Folder string `json:"folder"`
	UID    uint32 `json:"uid"`
	// Size is what the server said, -1 when it did not.
	Size int64 `json:"size"`
	// Detail is what went wrong, partly in the server's words: data,
	// never instructions.
	Detail   string    `json:"detail"`
	Attempts int       `json:"attempts"`
	FirstAt  time.Time `json:"first_at"`
	LastAt   time.Time `json:"last_at"`
}

// ManagedSource is a pull account's IMAP source as the settings show it.
type ManagedSource struct {
	State SourceState `json:"state"`
	// PausedAt is set while paused, for either reason.
	PausedAt *time.Time `json:"paused_at,omitempty"`
	Address  string     `json:"address"`
	Host     string     `json:"host"`
	Port     int        `json:"port"`
	Username string     `json:"username"`
	// Preset is gmail or absent.
	Preset SourcePreset `json:"preset,omitempty"`
	// Mailboxes are the folders synced; absent for the default.
	Mailboxes         []string `json:"mailboxes,omitempty"`
	TrustedAuthservID string   `json:"trusted_authserv_id,omitempty"`
	MaxMessageBytes   int64    `json:"max_message_bytes"`
	// BackfillSince is how far back the first sync goes; absent for all.
	BackfillSince *time.Time `json:"backfill_since,omitempty"`
	// DailyByteBudget is how much is fetched per day; 0 for no limit.
	DailyByteBudget int64 `json:"daily_byte_budget"`
	PollSeconds     int   `json:"poll_seconds"`
	// Secret is the password's state; absent once removed.
	Secret *SecretStatus `json:"secret,omitempty"`
	// Revision is what a change, pause, resume or removal names, so a
	// change made in between is refused (409) rather than overwritten.
	Revision   int64          `json:"revision"`
	CreatedAt  time.Time      `json:"created_at"`
	DisabledAt *time.Time     `json:"disabled_at,omitempty"`
	Folders    []SourceFolder `json:"folders"`
	// Failures are the messages not fetched yet.
	Failures []PullFailureView `json:"failures"`
}

// ManagedAccount is an account as the settings list it.
type ManagedAccount struct {
	ID   string      `json:"id"`
	Name string      `json:"name"`
	Kind AccountKind `json:"kind"`
	// DefaultRead is whether a principal without a setting of its own
	// may read the account.
	DefaultRead bool `json:"default_read"`
	// Access are the principals' own settings, by principal.
	Access    []AccessEntry `json:"access"`
	CreatedAt time.Time     `json:"created_at"`
	// Source is a pull account's; absent for a catch-all.
	Source *ManagedSource `json:"source,omitempty"`
}

// AdminAccountList is GET /api/v1/admin/accounts: every account by name,
// removed pull accounts included.
type AdminAccountList struct {
	Accounts []ManagedAccount `json:"accounts"`
	// Locked: the server has no key (§17.3), so no password can be set
	// and no source is pulled until it is back.
	Locked bool `json:"locked"`
}

// AccountBody is a pull account's whole configuration, as `yuuka account
// add` takes it. What is absent takes the preset's default.
type AccountBody struct {
	// Revision is the source's as read, 0 for a new account.
	Revision int64 `json:"revision" minimum:"0"`
	// Preset gmail: imap.gmail.com, \All only, a daily budget of 2 GiB.
	Preset SourcePreset `json:"preset,omitempty"`
	// Address is the mailbox's own: its mail is delivered to it, and it
	// is claimed as a prefix.
	Address string `json:"address"`
	// Host is the IMAP server (implicit TLS only); the preset's without
	// one.
	Host string `json:"host,omitempty"`
	// Port defaults to 993.
	Port int `json:"port,omitempty" maximum:"65535"`
	// Username defaults to the address.
	Username string `json:"username,omitempty"`
	// Password is write-only and never answered. A new account needs it;
	// absent keeps the one set, which works only while host, port and
	// user name stay (§17.4).
	Password string `json:"password,omitempty"`
	// TrustedAuthservID is whose Authentication-Results this mail
	// trusts; absent trusts none, so sender checks report nothing.
	TrustedAuthservID string `json:"trusted_authserv_id,omitempty"`
	// Mailboxes are the folders to sync; absent or empty for the default
	// (\All with gmail, else all but junk, trash and drafts).
	Mailboxes []string `json:"mailboxes,omitempty"`
	// BackfillSince is how far back the first sync goes; absent for all.
	BackfillSince *time.Time `json:"backfill_since,omitempty"`
	// MaxMessageBytes: larger mail is kept header only. Absent for the
	// largest yuuka reads back.
	MaxMessageBytes int64 `json:"max_message_bytes,omitempty" minimum:"0"`
	// DailyByteBudget is bytes fetched per day, 0 for no limit; absent
	// for the preset's.
	DailyByteBudget *int64 `json:"daily_byte_budget,omitempty" minimum:"0"`
	// PollSeconds is how often to look for new mail; absent for 60.
	PollSeconds int `json:"poll_seconds,omitempty" minimum:"0"`
}

// AccountNameParams name an account.
type AccountNameParams struct {
	Name string `path:"name" pattern:"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$"`
}

// AccountParams are the parameters of PUT /api/v1/admin/accounts/{name}.
type AccountParams struct {
	AccountNameParams
	Body AccountBody `body:"json"`
}

// AccountRevisionParams act on a source at the revision read.
type AccountRevisionParams struct {
	AccountNameParams
	Body RevisionBody `body:"json"`
}

// SyncBody asks for a round.
type SyncBody struct {
	// RetryFailed also fetches the messages that failed before.
	RetryFailed bool `json:"retry_failed,omitempty"`
}

// SyncParams are the parameters of POST /api/v1/admin/accounts/{name}/sync.
type SyncParams struct {
	AccountNameParams
	Body SyncBody `body:"json"`
}

// AccessSetting is a principal's setting for an account: read, no_read,
// or default (its own setting removed, so the account's default
// applies).
type AccessSetting string

func (AccessSetting) Enum() []string { return []string{"read", "no_read", "default"} }

// AccessBody changes one principal's setting.
type AccessBody struct {
	Access AccessSetting `json:"access"`
}

// AccessParams are the parameters of PUT /api/v1/admin/accounts/{name}/access/{principal}.
type AccessParams struct {
	AccountNameParams
	// Principal is kind:name.
	Principal string     `path:"principal"`
	Body      AccessBody `body:"json"`
}

// DefaultReadBody sets whether principals without a setting of their own
// may read the account.
type DefaultReadBody struct {
	DefaultRead bool `json:"default_read"`
}

// DefaultReadParams are the parameters of PUT /api/v1/admin/accounts/{name}/default-read.
type DefaultReadParams struct {
	AccountNameParams
	Body DefaultReadBody `body:"json"`
}

// ProbeFolder is a folder a source syncs, as STATUS saw it.
type ProbeFolder struct {
	Name        string `json:"name"`
	SpecialUse  string `json:"special_use,omitempty"`
	Messages    uint32 `json:"messages"`
	UIDNext     uint32 `json:"uid_next"`
	UIDValidity uint32 `json:"uidvalidity"`
}

// AccountProbe is what testing a source found: whether it logged in, and
// what it would sync. No mail is fetched and nothing changes on the
// server.
type AccountProbe struct {
	OK bool `json:"ok"`
	// LoginRefused: the server refused the password. Repeated wrong
	// passwords can lock the mailbox; Gmail needs an app password.
	LoginRefused bool `json:"login_refused,omitempty"`
	// Error says what failed, partly in the server's words.
	Error   string        `json:"error,omitempty"`
	Folders []ProbeFolder `json:"folders"`
	// Missing are configured folders the server does not have.
	Missing []string `json:"missing"`
	// Problems would stop a sync although the login worked.
	Problems []string `json:"problems"`
}
