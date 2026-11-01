package api

import "time"

// Transports, notification channels, bindings and the state of their
// sealed secrets (§17.6): configuration, served only to a person's
// session; widening it (a secret written, a destination added or moved)
// only to an elevated one. A secret is never answered, nor anything
// derived from it: a password or token field is write-only, and what is
// shown of a secret is whether one is set, when it was sealed and whether
// it opens.

// SecretState is whether a sealed secret opens: ok, or locked (§17.3).
type SecretState string

func (SecretState) Enum() []string { return []string{"ok", "locked"} }

// LockReason says why a secret is locked: no_key (the server has no key,
// YUUKA_KEK_FILE), key_missing (the key it was sealed under is not
// loaded, YUUKA_KEK_PREVIOUS_FILES), set_again (it does not open as
// sealed for its row: tampered, moved, or sealed by a newer yuuka; only
// setting it again helps).
type LockReason string

func (LockReason) Enum() []string { return []string{"no_key", "key_missing", "set_again"} }

// SecretStatus is all that is shown of a sealed secret.
type SecretStatus struct {
	SealedAt time.Time   `json:"sealed_at"`
	State    SecretState `json:"state"`
	// Reason is set when State is locked.
	Reason LockReason `json:"reason,omitempty"`
}

// TransportCap is something a transport lets yuuka set itself (§10.2):
// what is not listed is stripped from every message sent through it.
type TransportCap string

func (TransportCap) Enum() []string {
	return []string{"message-id", "return-path", "dkim", "status-query"}
}

// TransportState: configured; placeholder (a name identities use that
// has no destination yet, and cannot send); removed.
type TransportState string

func (TransportState) Enum() []string { return []string{"configured", "placeholder", "removed"} }

// ManagedTransport is a transport as the settings list it.
type ManagedTransport struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	State TransportState `json:"state"`
	// Host, Port, Username and Caps are absent for a placeholder.
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// Username is set when it authenticates (AUTH PLAIN).
	Username string         `json:"username,omitempty"`
	Caps     []TransportCap `json:"caps"`
	// Secret is the password's state, absent when none is set.
	Secret *SecretStatus `json:"secret,omitempty"`
	// Revision is what a change or removal names, so a change made in
	// between is refused (409) rather than overwritten.
	Revision     int64      `json:"revision"`
	CreatedAt    time.Time  `json:"created_at"`
	ConfiguredAt *time.Time `json:"configured_at,omitempty"`
	DisabledAt   *time.Time `json:"disabled_at,omitempty"`
	// UsedBy are the enabled identities whose chain names it.
	UsedBy []string `json:"used_by"`
}

// TransportList is GET /api/v1/admin/transports: every transport by
// name, removed ones included.
type TransportList struct {
	Transports []ManagedTransport `json:"transports"`
	// Locked: the server has no key (§17.3). No password can be set, and
	// none that is set opens: those transports hold their mail.
	Locked bool `json:"locked"`
}

// TransportBody is a transport's whole configuration, as `yuuka
// transport add` takes it.
type TransportBody struct {
	// Revision is the transport's as read, 0 for a name without one: a
	// new transport. A placeholder or a removed one is configured at its
	// revision.
	Revision int64 `json:"revision" minimum:"0"`
	// Host is the SMTP server; implicit TLS only.
	Host string `json:"host"`
	Port int    `json:"port" minimum:"1" maximum:"65535"`
	// Username turns on AUTH PLAIN; absent connects without it.
	Username string `json:"username,omitempty"`
	// Password is write-only and never answered. Absent keeps the one
	// set, which works only while host, port and user name stay: moving
	// the transport needs it again, since a password is only ever sent
	// where it was set for (§17.4).
	Password string `json:"password,omitempty"`
	// Caps are required, possibly empty: they decide which header
	// fields are stripped from every message, so there is no default.
	// Cloudflare Email Sending takes none; a submission server that
	// delivers what it is given takes message-id and return-path.
	Caps []TransportCap `json:"caps"`
}

// NameParams name a transport or a channel.
type NameParams struct {
	Name string `path:"name" pattern:"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$"`
}

// TransportParams are the parameters of PUT /api/v1/admin/transports/{name}.
type TransportParams struct {
	NameParams
	Body TransportBody `body:"json"`
}

// RevisionBody names the revision a removal is made at.
type RevisionBody struct {
	Revision int64 `json:"revision" minimum:"1"`
}

// RemoveParams are the parameters of a removal by name.
type RemoveParams struct {
	NameParams
	Body RevisionBody `body:"json"`
}

// TransportRemoved answers a transport's removal.
type TransportRemoved struct {
	// StillNamedBy are the enabled identities whose chain still names
	// it: their mail needs another transport in the chain.
	StillNamedBy []string `json:"still_named_by"`
}

// TransportProbe is what testing a transport found: whether the server
// answered EHLO and, with a user name, accepted AUTH PLAIN. No mail is
// sent.
type TransportProbe struct {
	OK bool `json:"ok"`
	// Error says what failed when not OK.
	Error string `json:"error,omitempty"`
}

// ChainBody is an identity's transport chain, primary first.
type ChainBody struct {
	Transports []string `json:"transports"`
}

// ChainParams are the parameters of PUT /api/v1/admin/identities/{id}/transports.
type ChainParams struct {
	ID   string    `path:"id"`
	Body ChainBody `body:"json"`
}

// ManagedChannel is a notification channel as the settings list it.
type ManagedChannel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind is telegram, the only one so far.
	Kind string `json:"kind"`
	// BotID is the Telegram bot's id; BotUsername its @name, without @.
	BotID       string `json:"bot_id"`
	BotUsername string `json:"bot_username,omitempty"`
	// Secret is the bot token's state, absent once removed.
	Secret     *SecretStatus `json:"secret,omitempty"`
	Revision   int64         `json:"revision"`
	CreatedAt  time.Time     `json:"created_at"`
	DisabledAt *time.Time    `json:"disabled_at,omitempty"`
	// Bindings is how many live bindings it has, everyone's.
	Bindings int `json:"bindings"`
}

// ChannelList is GET /api/v1/admin/channels, removed ones included.
type ChannelList struct {
	Channels []ManagedChannel `json:"channels"`
	// Locked: the server has no key; no channel runs (§17.3).
	Locked bool `json:"locked"`
}

// ChannelBody gives a channel its bot token.
type ChannelBody struct {
	// Revision is the channel's as read, 0 for a new name. A removed
	// channel is enabled again at its revision; its bindings do not
	// come back.
	Revision int64 `json:"revision" minimum:"0"`
	// Token is the bot token from @BotFather: write-only, never
	// answered. Telegram is asked whose it is; an enabled channel keeps
	// its bot.
	Token string `json:"token"`
}

// ChannelParams are the parameters of PUT /api/v1/admin/channels/{name}.
type ChannelParams struct {
	NameParams
	Body ChannelBody `body:"json"`
}

// ChannelTestResult is how many bindings a test message reached.
type ChannelTestResult struct {
	Sent int `json:"sent"`
	// Error says what failed for the others.
	Error string `json:"error,omitempty"`
}

// Event is what a binding can subscribe to (§8.4). Security alerts are
// not one: they go to administrators and the person they are about,
// always.
type Event string

func (Event) Enum() []string {
	return []string{"mail.new", "send.held", "send.escalated", "grant.suspended"}
}

// MyBinding is one of the viewer's own bindings: a chat that receives
// their notifications.
type MyBinding struct {
	ID          string `json:"id"`
	Channel     string `json:"channel"`
	BotUsername string `json:"bot_username,omitempty"`
	// Chat is the Telegram chat id.
	Chat string `json:"chat"`
	// ButtonUser is the Telegram user whose button presses count as
	// the viewer's; absent for a binding without buttons.
	ButtonUser string     `json:"button_user,omitempty"`
	Label      string     `json:"label"`
	Events     []Event    `json:"events"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// BindableChannel is an enabled channel a chat can be bound on.
type BindableChannel struct {
	Name        string `json:"name"`
	BotUsername string `json:"bot_username,omitempty"`
}

// BindingList is GET /api/v1/bindings: the viewer's live bindings, and
// what a new one can be made on.
type BindingList struct {
	Bindings []MyBinding       `json:"bindings"`
	Channels []BindableChannel `json:"channels"`
	// Locked: the server has no key, so no channel runs and nothing can
	// be bound (§17.3).
	Locked bool `json:"locked"`
}

// EventsBody is what a binding receives from now on.
type EventsBody struct {
	Events []Event `json:"events"`
}

// EventsParams are the parameters of PUT /api/v1/bindings/{id}/events.
type EventsParams struct {
	ID   string     `path:"id"`
	Body EventsBody `body:"json"`
}

// BindStartBody starts binding a chat to the viewer.
type BindStartBody struct {
	Channel string `json:"channel"`
}

// BindStartParams are the parameters of POST /api/v1/bind-codes.
type BindStartParams struct {
	Body BindStartBody `body:"json"`
}

// BindCodeState: waiting for the code to reach the bot; redeemed (who
// sent it is in Seen, to confirm or decline); confirmed; expired.
type BindCodeState string

func (BindCodeState) Enum() []string { return []string{"waiting", "redeemed", "confirmed", "expired"} }

// SeenIdentity is who sent the code to the bot, as Telegram reported
// it. Nothing is granted until the person the code is for confirms it
// (§13.7).
type SeenIdentity struct {
	// Label is the name Telegram gave: a person's name and @username,
	// or a group's title. Untrusted text, chosen by whoever sent it.
	Label string `json:"label"`
	// UserID is the Telegram user whose presses would count; ChatID the
	// chat that would receive the notifications.
	UserID string `json:"user_id"`
	ChatID string `json:"chat_id"`
	// ChatKind is Telegram's: private, group or supergroup.
	ChatKind string    `json:"chat_kind"`
	At       time.Time `json:"at"`
}

// BindCodeView is a bind code of the viewer's, never its text.
type BindCodeView struct {
	ID          string        `json:"id"`
	Channel     string        `json:"channel"`
	BotUsername string        `json:"bot_username,omitempty"`
	State       BindCodeState `json:"state"`
	ExpiresAt   time.Time     `json:"expires_at"`
	Seen        *SeenIdentity `json:"seen,omitempty"`
	// BindingID is the binding made once confirmed.
	BindingID string `json:"binding_id,omitempty"`
}

// BindStart answers a new bind code. Code is shown this once; only its
// hash is stored. It is sent to the bot by opening Link in the Telegram
// account that should be notified (GroupLink for a group), or as
// "/start <code>" when the bot's name is unknown.
type BindStart struct {
	BindCode  BindCodeView `json:"bind_code"`
	Code      string       `json:"code"`
	Link      string       `json:"link,omitempty"`
	GroupLink string       `json:"group_link,omitempty"`
}

// BindConfirmBody confirms the identity a code's sender was reported as.
type BindConfirmBody struct {
	// Label names the binding; absent takes Telegram's name for it.
	Label string `json:"label,omitempty"`
	// Events are what it receives; absent is every event, [] none.
	Events []Event `json:"events,omitempty"`
}

// BindConfirmParams are the parameters of POST /api/v1/bind-codes/{id}/confirm.
type BindConfirmParams struct {
	ID   string          `path:"id"`
	Body BindConfirmBody `body:"json"`
}

// SecretOwner is what a sealed secret is for: the transport, channel or
// pull account that uses it, and where it may be sent.
type SecretOwner struct {
	// Kind is transport, channel or account; Name the row's.
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Destination is where the secret may go: user@host:port (a
	// transport's SMTP server, a pull account's IMAP server), or the bot
	// as @name (id).
	Destination string `json:"destination"`
}

// ManagedSecret is a sealed secret's state, with what it is for.
type ManagedSecret struct {
	Owner SecretOwner `json:"owner"`
	SecretStatus
}

// SecretsStatus is GET /api/v1/admin/secrets: whether the server can seal
// and open third-party secrets at all, and the state of each one that is
// in use.
type SecretsStatus struct {
	// Locked: the server has no key (YUUKA_KEK_FILE). Mail is received
	// as ever; transports with a password hold their mail and no channel
	// runs until the key is back (§17.3).
	Locked  bool            `json:"locked"`
	Secrets []ManagedSecret `json:"secrets"`
}
