// Package api serves the REST API under /api/v1/ for the WebUI (session
// cookie) and for agents and services (bearer token). Every route states
// the scopes it needs, and every store read takes the request's viewer, so
// visibility (§13.8) is decided by the store and nowhere here.
//
// The route table is the single source of the contract (§5.1): it serves
// the requests, the ACL test walks it, and openapi.json is generated from
// it and the types in types.go (go generate ./internal/api).
package api

//go:generate go run ./openapigen -o openapi.json

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/classify"
	"github.com/yuuka-mail/yuuka/internal/leak"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/rules"
	"github.com/yuuka-mail/yuuka/internal/search"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/submit"
	"github.com/yuuka-mail/yuuka/internal/triage"
	"github.com/yuuka-mail/yuuka/internal/triage/score"
)

// Store is the slice of the store the API uses. Every read and every
// draft write takes the viewer; the ACL test wraps the real store to
// check that it arrives.
type Store interface {
	ListMessages(ctx context.Context, v *auth.Viewer, q store.MessageQuery) ([]store.MessageSummary, error)
	MessageByID(ctx context.Context, v *auth.Viewer, id string) (store.MessageDetail, error)
	ThreadByID(ctx context.Context, v *auth.Viewer, id string) (store.ThreadView, error)
	PrefixStats(ctx context.Context, v *auth.Viewer, q store.PrefixQuery) (store.PrefixSummary, error)
	PrefixByAddress(ctx context.Context, v *auth.Viewer, zone, prefix string, limit int) (store.PrefixDetail, error)
	Search(ctx context.Context, v *auth.Viewer, q store.SearchQuery) (store.SearchResult, error)
	Changes(ctx context.Context, v *auth.Viewer, q store.ChangeQuery) (store.ChangePage, error)
	// Scores (§15) go where their message goes; what the scorer was
	// shown only to a viewer of every delivery (LatestScore).
	LatestScore(ctx context.Context, v *auth.Viewer, id string, p score.Profile) (*store.ScoreRead, error)
	TopAnswers(ctx context.Context, v *auth.Viewer, p score.Profile, ids []string) (map[string]map[string]string, error)
	ScoreCounts(ctx context.Context, v *auth.Viewer, p score.Profile, since time.Time) (store.ScoreCounts, error)
	// RecordAction appends to the audit log (attachment downloads).
	RecordAction(ctx context.Context, a store.Action) error

	DraftByID(ctx context.Context, v *auth.Viewer, id string) (store.Draft, error)
	Drafts(ctx context.Context, v *auth.Viewer, q store.DraftQuery) ([]store.Draft, error)
	CreateDraft(ctx context.Context, v *auth.Viewer, d store.Draft, at time.Time) (store.Draft, error)
	UpdateDraft(ctx context.Context, v *auth.Viewer, d store.Draft, at time.Time) (store.Draft, error)
	DeleteDraft(ctx context.Context, v *auth.Viewer, id string, at time.Time) error
	ReplyIdentities(ctx context.Context, v *auth.Viewer, id string) ([]store.ReplyIdentity, error)
	Standings(ctx context.Context, v *auth.Viewer) ([]store.Standing, error)
	OutboxList(ctx context.Context, v *auth.Viewer, q store.OutboxQuery) ([]store.OutboxEntry, error)
	OutboxByID(ctx context.Context, v *auth.Viewer, id string) (store.OutboxEntry, error)
	// Subjects names the messages a review verdict refers to, those v
	// may see only; Counts is the sidebar's, each over what its list
	// would show v.
	Subjects(ctx context.Context, v *auth.Viewer, ids []string) (map[string]string, error)
	Counts(ctx context.Context, v *auth.Viewer, now time.Time) (store.Counts, error)
	// ResolveOutbox records a person's decision on an unknown row; the
	// handler reads the row as the viewer first (OutboxByID).
	ResolveOutbox(ctx context.Context, id, resolution string, a store.Action, p store.RetryPolicy) (string, error)
	// WithdrawOutbox recalls held mail as v; who may recall what is the
	// store's (ErrNotYours).
	WithdrawOutbox(ctx context.Context, v *auth.Viewer, id string, via map[string]any, at time.Time) (store.Withdrawal, error)

	// Annotations and proposals (§8.5) take the viewer; precedence and
	// who may decide are the store's too.
	Annotate(ctx context.Context, v *auth.Viewer, id string, ins []store.Annotation, a store.Action) ([]store.AnnotationResult, bool, error)
	AnnotateThread(ctx context.Context, v *auth.Viewer, id string, ins []store.Annotation, a store.Action) (map[string][]store.AnnotationResult, bool, error)
	Undo(ctx context.Context, v *auth.Viewer, action string, a store.Action) (store.UndoResult, error)
	MessageAnnotations(ctx context.Context, v *auth.Viewer, id string) ([]store.AnnotationEntry, error)
	Labels(ctx context.Context, v *auth.Viewer) ([]store.LabelCount, error)
	CreateProposal(ctx context.Context, v *auth.Viewer, p store.NewProposal, a store.Action) (store.Proposal, []rules.Warning, error)
	Proposals(ctx context.Context, v *auth.Viewer, q store.ProposalQuery) ([]store.Proposal, error)
	ProposalByID(ctx context.Context, v *auth.Viewer, id string) (store.Proposal, error)
	AcceptProposal(ctx context.Context, v *auth.Viewer, id string, rule *store.NewRule, a store.Action) (store.Accepted, error)
	RejectProposal(ctx context.Context, v *auth.Viewer, id string, a store.Action) error
	WithdrawProposal(ctx context.Context, v *auth.Viewer, id string, a store.Action) error
	// Notes (§8.10): a person reads every note on visible mail, anyone
	// else its own; only the author writes one, and deletes it, as does
	// a person.
	Notes(ctx context.Context, v *auth.Viewer, id string) ([]store.Note, error)
	WriteNote(ctx context.Context, v *auth.Viewer, n store.NewNote, a store.Action) (store.Note, bool, error)
	DeleteNote(ctx context.Context, v *auth.Viewer, message, scope, author string, a store.Action) error

	// Rules are the operator's configuration: only a person manages
	// them, which the handlers check, and their reads take no viewer.
	// What they run over past mail is all of it; a preview and a draft
	// read what the viewer can see.
	Rules(ctx context.Context, all bool) ([]store.Rule, error)
	RuleByID(ctx context.Context, id string) (store.Rule, error)
	CreateRule(ctx context.Context, n store.NewRule, a store.Action) (store.Rule, []rules.Warning, error)
	UpdateRule(ctx context.Context, id string, u store.RuleUpdate, a store.Action) (store.Rule, []rules.Warning, error)
	DeleteRule(ctx context.Context, id string, a store.Action) error
	ApplyRule(ctx context.Context, id string, since time.Time, dry bool, a store.Action) (store.RetroStats, error)
	RevertRule(ctx context.Context, id string, since time.Time, a store.Action) (store.RevertStats, error)
	PreviewRule(ctx context.Context, v *auth.Viewer, spec []byte, q store.PreviewQuery) (store.Preview, error)
	FactsFor(ctx context.Context, v *auth.Viewer, id string) ([]rules.Facts, error)
	CountMatches(ctx context.Context, v *auth.Viewer, ms []rules.Match, since time.Time) ([]int, error)

	// Decisions prompted by mail (§13.10), the same functions `yuuka
	// prefix` calls; what they mean for the mail is read back through
	// PrefixStats as the viewer.
	SetPrefixState(ctx context.Context, zone, prefix string, state int, a store.Action) (store.PrefixChange, error)
	SetPrefixExpected(ctx context.Context, zone, prefix string, domains []string, a store.Action) ([]string, bool, error)

	// Who may do what (§13.10): configuration, read without a viewer;
	// the routes let only a person's session through, and only an
	// elevated one widen. The same functions the host CLI calls.
	Tokens(ctx context.Context, principalID string) ([]store.CredentialPrincipal, error)
	Principal(ctx context.Context, id string) (store.Principal, error)
	Grantee(ctx context.Context, kind, name string, a store.Action) (store.Principal, bool, error)
	CreateCredential(ctx context.Context, c store.Credential, a store.Action) error
	Accounts(ctx context.Context) ([]store.Account, error)
	AddIdentity(ctx context.Context, i store.Identity, a store.Action) error
	Transports(ctx context.Context, all bool) ([]store.Transport, error)
	DisableIdentity(ctx context.Context, id string, a store.Action) error
	GrantFor(ctx context.Context, identityID, principalID string) (store.Grant, error)
	SetGrant(ctx context.Context, g store.Grant, a store.Action) error
	RemoveGrant(ctx context.Context, identityID, principalID string, a store.Action) error
	CredentialForAuth(ctx context.Context, id string) (store.CredentialPrincipal, error)
	RevokeCredential(ctx context.Context, id string, a store.Action) error
	Identities(ctx context.Context, all bool) ([]store.Identity, error)
	IdentityByID(ctx context.Context, id string) (store.Identity, error)
	DisableIdentityIfTighter(ctx context.Context, id string, a store.Action) error
	Grants(ctx context.Context, identityID, principalID string) ([]store.Grant, error)
	TightenGrant(ctx context.Context, identityID, principalID string, t store.GrantTightening, a store.Action) (store.Grant, error)
	RemoveGrantIfTighter(ctx context.Context, identityID, principalID string, a store.Action) (sendpolicy.Grant, error)
}

// RawReader loads the raw message of a delivery (ingest.Ingester).
type RawReader interface {
	ReadRaw(ctx context.Context, ref store.RawRef) ([]byte, error)
}

// Guard authenticates a request and checks its scopes before h runs with
// the viewer in the context (authn.Service.Require).
type Guard interface {
	Require(h http.Handler, scopes ...auth.Scope) http.Handler
}

// SuspendNotifier tells a person that a recall tripped the circuit
// breaker of a grant (§8.4, §8.9). It is told after the recall commits;
// failing to tell is its own to log, and changes nothing.
type SuspendNotifier interface {
	Suspended(ctx context.Context, s store.Suspension)
}

// API is the REST handler.
type API struct {
	db        Store
	raw       RawReader
	submit    Submitter
	suspended SuspendNotifier
	alert     authn.Alerter
	// triage is the active scorer profile (§15), nil when no scorer is
	// configured: then no score is shown, filtered by or reported.
	triage *score.Profile
	log    *slog.Logger
}

// New returns the API over db, reading raw messages for the structured
// view through raw.
func New(db Store, raw RawReader, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{db: db, raw: raw, log: log}
}

// WithSubmitter makes a submit drafts through s. Without one the submit
// route is not served at all: there is no other way into the outbox.
func (a *API) WithSubmitter(s Submitter) *API {
	a.submit = s
	return a
}

// WithTriage makes a show the scores of profile p (§15): the profile
// of the configured scorer, whose scores are the current ones.
func (a *API) WithTriage(p score.Profile) *API {
	a.triage = &p
	return a
}

// WithAlerter makes a tell the operator through n whenever a session
// widens who may do what (§13.10), as a new passkey is told.
func (a *API) WithAlerter(n authn.Alerter) *API {
	a.alert = n
	return a
}

// WithSuspendNotifier makes a tell n of grants a recall suspended.
func (a *API) WithSuspendNotifier(n SuspendNotifier) *API {
	a.suspended = n
	return a
}

var (
	read   = []auth.Scope{auth.MessageRead}
	prefix = []auth.Scope{auth.PrefixRead}
	// draft:create writes drafts and submits them. Submitting needs no
	// more: the core decides, and a credential without submission:send
	// gets a draft held for policy rather than an error (§8.9).
	draftWrite = []auth.Scope{auth.DraftCreate}
	// The outbox holds envelope recipients, Bcc included: delivery data.
	outboxRead = []auth.Scope{auth.MessageRead, auth.DeliveryRead}
	// Resolving resends: a sending decision, and a person's (§4.5).
	outboxResolve = []auth.Scope{auth.SubmissionSend}
	// Withdrawing held mail is a sending decision too, the opposite one
	// (§8.9); it can only stop mail, never send it sooner.
	outboxWithdraw = []auth.Scope{auth.SubmissionSend}
	// Annotating, undoing one's annotations, proposing and deciding
	// proposals (§8.5). Deciding is a person's, checked by kind.
	annotate = []auth.Scope{auth.MessageAnnotate}
	// Rules are a person's, checked by kind; previews and drafts list
	// mail as well.
	ruleManage  = []auth.Scope{auth.RuleManage}
	ruleReading = []auth.Scope{auth.RuleManage, auth.MessageRead}
	// Claiming a prefix and what it expects (§3.3); the answer is the
	// prefix as read, so reading it too.
	prefixManage = []auth.Scope{auth.PrefixManage, auth.PrefixRead}
	// Who may do what (§13.10): routes marked authority(), a person's
	// session only.
	admin = []auth.Scope{auth.Admin}
)

// routes is every operation under /api/. A new one is one entry here and
// a handler; the ACL test and openapi.json pick it up from this table.
var routes = []route{
	jsonRoute(http.MethodGet, "/api/v1/messages", "List messages, newest first", read, (*API).listMessages),
	jsonRoute(http.MethodGet, "/api/v1/messages/{id}", "Get a message's parsed fields and plain text body", read, (*API).message),
	jsonRoute(http.MethodGet, "/api/v1/messages/{id}/view", "Get the structured view of a message (§8.1)", read, (*API).messageView),
	rawRoute(http.MethodGet, "/api/v1/messages/{id}/attachments/{part}", "Download an attachment",
		"application/octet-stream", read, (*API).attachment),
	jsonRoute(http.MethodGet, "/api/v1/threads/{id}", "Get a thread", read, (*API).thread),
	jsonRoute(http.MethodGet, "/api/v1/prefixes", "List prefixes with message counts", prefix, (*API).prefixes),
	// The offending mail is listed as messages, so this needs both.
	jsonRoute(http.MethodGet, "/api/v1/prefixes/{address}", "Get a prefix with its leak check and the latest mail that leaked or spoofed",
		[]auth.Scope{auth.PrefixRead, auth.MessageRead}, (*API).prefix),
	jsonRoute(http.MethodGet, "/api/v1/search", "Search messages, newest first", read, (*API).search),
	jsonRoute(http.MethodGet, "/api/v1/triage/report",
		"Count how the latest scores of the configured scorer answer, over the mail the caller sees (§15); people only",
		read, (*API).triageReport),
	// Following the log of an object kind needs that kind's scopes too
	// (agentview.ChangeKind.Scopes), checked by the handler.
	jsonRoute(http.MethodGet, "/api/v1/changes", "List what changed after a cursor, old mail included, in commit order (§8.6)",
		read, (*API).changes),
	jsonRoute(http.MethodGet, "/api/v1/messages/{id}/reply-identities", "List the identities a reply defaults to, best first",
		[]auth.Scope{auth.MessageRead, auth.DraftCreate}, (*API).replyIdentities),
	jsonRoute(http.MethodGet, "/api/v1/identities", "List the identities the viewer may send as, and how it would go",
		draftWrite, (*API).identities),
	jsonRoute(http.MethodGet, "/api/v1/drafts", "List drafts, most recently changed first", read, (*API).drafts),
	jsonRoute(http.MethodPost, "/api/v1/drafts", "Create a draft", draftWrite, (*API).createDraft).withStatus(http.StatusCreated),
	jsonRoute(http.MethodGet, "/api/v1/drafts/{id}", "Get a draft", read, (*API).draft),
	jsonRoute(http.MethodPut, "/api/v1/drafts/{id}", "Replace a draft's content", draftWrite, (*API).updateDraft),
	jsonRoute(http.MethodDelete, "/api/v1/drafts/{id}", "Delete a draft", draftWrite, (*API).deleteDraft),
	jsonRoute(http.MethodPost, "/api/v1/drafts/{id}/submit", "Submit a draft: queued, or held as a draft with the reason (§8.9)",
		draftWrite, (*API).submitDraft).submitting(),
	jsonRoute(http.MethodGet, "/api/v1/outbox", "List outgoing mail, newest first", outboxRead, (*API).outboxList),
	jsonRoute(http.MethodGet, "/api/v1/outbox/{id}", "Get one outgoing message's state", outboxRead, (*API).outboxEntry),
	jsonRoute(http.MethodPost, "/api/v1/outbox/{id}/resolve", "Decide on mail whose delivery is unknown (§4.5); people only",
		outboxResolve, (*API).resolveOutbox),
	// Recall only: no route releases held mail or shortens its window.
	jsonRoute(http.MethodPost, "/api/v1/outbox/{id}/withdraw",
		"Withdraw held mail during its recall window, back to its draft (§8.9); agents and services only their own",
		outboxWithdraw, (*API).withdrawOutbox),

	jsonRoute(http.MethodGet, "/api/v1/labels", "List the labels in use, with how many messages carry each", read, (*API).labels),
	jsonRoute(http.MethodGet, "/api/v1/counts", "Count what waits in the outbox, proposals and drafts, as their lists would show it",
		read, (*API).counts),
	jsonRoute(http.MethodPost, "/api/v1/messages/{id}/annotations",
		"Label, classify or archive a message or its thread; a higher-ranked writer's value is overruled, not an error",
		annotate, (*API).annotate),
	jsonRoute(http.MethodGet, "/api/v1/messages/{id}/annotations", "Get where a message's labels, class and archived state came from",
		read, (*API).annotations),
	jsonRoute(http.MethodPost, "/api/v1/actions/{id}/undo", "Undo the annotations an action wrote; agents undo only their own",
		annotate, (*API).undo),
	jsonRoute(http.MethodGet, "/api/v1/proposals", "List proposals, newest first; agents see only their own", read, (*API).proposals),
	jsonRoute(http.MethodPost, "/api/v1/proposals", "Propose annotations or a rule for a person to decide",
		annotate, (*API).createProposal).withStatus(http.StatusCreated),
	jsonRoute(http.MethodGet, "/api/v1/proposals/{id}", "Get a proposal", read, (*API).proposal),
	jsonRoute(http.MethodPost, "/api/v1/proposals/{id}/accept",
		"Accept a proposal, optionally creating a rule from it (rule:manage); people only", annotate, (*API).acceptProposal),
	jsonRoute(http.MethodPost, "/api/v1/proposals/{id}/reject", "Reject a proposal; people only", annotate, (*API).rejectProposal),
	jsonRoute(http.MethodPost, "/api/v1/proposals/{id}/withdraw", "Withdraw one's own proposal", annotate, (*API).withdrawProposal),
	// Reading notes is part of GET /api/v1/messages/{id}.
	jsonRoute(http.MethodPut, "/api/v1/messages/{id}/note",
		"Write one's note on a message or its thread, replacing one's earlier note there; untrusted, it drives nothing",
		annotate, (*API).writeNote),
	jsonRoute(http.MethodDelete, "/api/v1/messages/{id}/note",
		"Delete one's note on a message or its thread; a person may delete anyone's",
		annotate, (*API).deleteNote),
	jsonRoute(http.MethodGet, "/api/v1/rules", "List rules in evaluation order; people only", ruleManage, (*API).rules),
	jsonRoute(http.MethodPost, "/api/v1/rules", "Create a rule; it runs on mail arriving from now on; people only",
		ruleManage, (*API).createRule).withStatus(http.StatusCreated),
	jsonRoute(http.MethodPost, "/api/v1/rules/preview", "Try a rule on the mail the caller can see without saving it; people only",
		ruleReading, (*API).previewRule),
	jsonRoute(http.MethodPost, "/api/v1/rules/draft",
		"Draft the conditions of a rule for mail like an example message, with match counts; people only",
		ruleReading, (*API).draftRule),
	jsonRoute(http.MethodGet, "/api/v1/rules/{id}", "Get a rule, deleted or not; people only", ruleManage, (*API).rule),
	jsonRoute(http.MethodPut, "/api/v1/rules/{id}", "Replace a rule; people only", ruleManage, (*API).updateRule),
	jsonRoute(http.MethodDelete, "/api/v1/rules/{id}", "Delete a rule; what it did stays until reverted; people only",
		ruleManage, (*API).deleteRule),
	jsonRoute(http.MethodPost, "/api/v1/rules/{id}/apply", "Run a rule over past mail; it never notifies; people only",
		ruleManage, (*API).applyRule),
	jsonRoute(http.MethodPost, "/api/v1/rules/{id}/revert", "Take back what a rule did to mail; people only",
		ruleManage, (*API).revertRule),

	jsonRoute(http.MethodPost, "/api/v1/prefixes/{address}/claim",
		"Claim a prefix: its mail is no longer downgraded unless DMARC failed, and new mail to it notifies (§3.3)",
		prefixManage, (*API).claimPrefix),
	jsonRoute(http.MethodPost, "/api/v1/prefixes/{address}/unclaim", "Unclaim a prefix: its mail is downgraded again",
		prefixManage, (*API).unclaimPrefix),
	jsonRoute(http.MethodPut, "/api/v1/prefixes/{address}/expected-domains",
		"Replace the domains a claimed prefix was given to, and see what the leak check now makes of its mail",
		prefixManage, (*API).expectDomains),

	jsonRoute(http.MethodGet, "/api/v1/admin/credentials", "List tokens and SMTP passwords, never their secrets; a person's session only",
		admin, (*API).credentials).authority(),
	jsonRoute(http.MethodPost, "/api/v1/admin/credentials", "Issue a token or SMTP password, shown once; an elevated session only",
		admin, (*API).issueCredential).widening().withStatus(http.StatusCreated),
	jsonRoute(http.MethodPost, "/api/v1/admin/credentials/{id}/revoke", "Revoke a token or SMTP password; a person's session only",
		admin, (*API).revokeCredential).authority(),
	jsonRoute(http.MethodGet, "/api/v1/admin/identities", "List identities, disabled ones included; a person's session only",
		admin, (*API).adminIdentities).authority(),
	jsonRoute(http.MethodPost, "/api/v1/admin/identities", "Add an identity; an elevated session only",
		admin, (*API).addIdentity).widening().withStatus(http.StatusCreated),
	jsonRoute(http.MethodPost, "/api/v1/admin/identities/{id}/disable",
		"Disable an identity; where the zone's pattern would take its address over, an elevated session only",
		admin, (*API).disableIdentity).authority(),
	jsonRoute(http.MethodGet, "/api/v1/admin/grants", "List grants, stored and applied; a person's session only",
		admin, (*API).adminGrants).authority(),
	jsonRoute(http.MethodPut, "/api/v1/admin/grants", "Create or replace a grant, wider or not; an elevated session only",
		admin, (*API).setGrant).widening(),
	jsonRoute(http.MethodPost, "/api/v1/admin/grants/{identity}/{principal}/tighten",
		"Tighten a grant; anything that would let more through is refused; a person's session only",
		admin, (*API).tightenGrant).authority(),
	jsonRoute(http.MethodDelete, "/api/v1/admin/grants/{identity}/{principal}",
		"Remove a grant; where its default lets more through, an elevated session only",
		admin, (*API).removeGrant).authority(),
	{Op: openapi.Op{
		Method: http.MethodGet, Path: "/api/v1/openapi.json", Summary: "This document",
		Access: openapi.Public, Media: "application/json",
	}},
}

// OpenAPI is the document of routes and of the /auth/ endpoints.
var OpenAPI = sync.OnceValues(func() ([]byte, error) {
	ops := authn.Ops()
	for _, rt := range routes {
		ops = append(ops, rt.Op)
	}
	return openapi.Document("yuuka", "v1", ops)
})

// Handler serves /api/. Unknown paths under it are problem+json 404s,
// never the WebUI shell.
func (a *API) Handler(g Guard) http.Handler {
	doc, err := OpenAPI()
	if err != nil {
		panic(err) // the types cannot describe themselves: TestOpenAPI fails first
	}
	mux := http.NewServeMux()
	for _, rt := range routes {
		pattern := rt.Method + " " + rt.Path
		if rt.needsSubmitter && a.submit == nil {
			continue // answered by the 404 below
		}
		if rt.serve == nil {
			// The contract is public: the repository is, and an agent
			// with a token can read what it may call.
			mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(doc)
			})
			continue
		}
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			v, ok := auth.ViewerFrom(r.Context())
			if !ok {
				// Guard let a request through without a viewer: a wiring
				// bug, never "anonymous".
				a.fail(w, r, errors.New("api: no viewer after guard"))
				return
			}
			if rt.Access == openapi.HumanSession || rt.Access == openapi.ElevatedSession {
				if err := authorityDenied(v); err != nil {
					a.fail(w, r, err)
					return
				}
			}
			if rt.Access == openapi.ElevatedSession && !v.Elevated(time.Now()) {
				a.fail(w, r, elevationRequired("This lets someone do more than now."))
				return
			}
			res, err := rt.serve(a, w, r, v)
			switch {
			case err != nil:
				a.fail(w, r, err)
			case rt.Response != nil:
				writeJSON(w, openapi.StatusOf(rt.Op), res)
			case rt.Media == "":
				w.WriteHeader(openapi.StatusOf(rt.Op)) // 204
			}
		})
		mux.Handle(pattern, g.Require(h, rt.Scopes...))
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		openapi.Write(w, openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "no endpoint %s %s", r.Method, r.URL.Path))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Mail is private; no shared or browser cache keeps a copy.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// fail maps an error to a problem: one a handler or bind returned as is,
// the store's and the submission core's sentinels to their codes,
// anything else to internal.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	if p, ok := errors.AsType[*openapi.Problem](err); ok {
		openapi.Write(w, p)
		return
	}
	if e, ok := errors.AsType[*search.Error](err); ok {
		p := openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidParam, "q: %s", e)
		switch {
		case e.Scope != "":
			p = openapi.Errorf(http.StatusForbidden, openapi.CodeScopeRequired, "q: %s", e)
		case e.Unsupported:
			p = openapi.Errorf(http.StatusBadRequest, openapi.CodeUnsupportedFilter, "q: %s", e)
		}
		openapi.Write(w, p)
		return
	}
	if e, ok := errors.AsType[*rules.InvalidError](err); ok {
		p := openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidRule, "%s", e.Reason)
		p.Path = e.Path
		openapi.Write(w, p)
		return
	}
	for _, m := range []struct {
		err    error
		status int
		code   openapi.Code
		detail string // "" for the error's own text, which names no mail content
	}{
		{store.ErrNotFound, http.StatusNotFound, openapi.CodeNotFound, "not found"},
		{store.ErrInvalidDraft, http.StatusBadRequest, openapi.CodeInvalidBody, ""},
		{store.ErrInFlight, http.StatusConflict, openapi.CodeConflict, "the draft has gone to the outbox"},
		{store.ErrRawDraft, http.StatusConflict, openapi.CodeConflict, ""},
		{store.ErrOutboxState, http.StatusConflict, openapi.CodeConflict, ""},
		{store.ErrNotYours, http.StatusForbidden, openapi.CodeForbidden, ""},
		{store.ErrNoIdentity, http.StatusUnprocessableEntity, openapi.CodeNoIdentity, ""},
		{submit.ErrNoIdentity, http.StatusUnprocessableEntity, openapi.CodeNoIdentity, ""},
		{submit.ErrRejected, http.StatusForbidden, openapi.CodeSubmissionRejected, ""},
		{rules.ErrInvalid, http.StatusBadRequest, openapi.CodeInvalidRule, ""},
		{rules.ErrUnsupported, http.StatusConflict, openapi.CodeConflict, ""},
		{store.ErrInvalidAnnotation, http.StatusBadRequest, openapi.CodeInvalidBody, ""},
		{store.ErrNotPermitted, http.StatusForbidden, openapi.CodeForbidden, ""},
		{store.ErrNotPending, http.StatusConflict, openapi.CodeConflict, "the proposal is no longer pending"},
		{store.ErrDuplicate, http.StatusConflict, openapi.CodeConflict, "the same proposal is pending already"},
		{store.ErrInvalidNote, http.StatusBadRequest, openapi.CodeInvalidBody, ""},
		{store.ErrTooManyProposals, http.StatusTooManyRequests, openapi.CodeRateLimited, "too many pending proposals; withdraw some or wait for a decision"},
		{store.ErrRuleChanged, http.StatusConflict, openapi.CodeConflict, ""},
		{store.ErrCursorExpired, http.StatusGone, openapi.CodeCursorExpired, "the cursor cannot be continued; start again without since and reread what you hold"},
	} {
		if errors.Is(err, m.err) {
			detail := m.detail
			if detail == "" {
				detail = err.Error()
			}
			openapi.Write(w, openapi.Errorf(m.status, m.code, "%s", detail))
			return
		}
	}
	if r.Context().Err() != nil {
		return // the client went away
	}
	a.log.Error("api", "path", r.URL.Path, "err", err)
	openapi.Write(w, openapi.Errorf(http.StatusInternalServerError, openapi.CodeInternal, "internal error"))
}

var downgrades = map[Downgraded]store.Downgrade{
	"hide": store.HideDowngraded, "include": store.IncludeDowngraded, "only": store.OnlyDowngraded,
}

var classes = map[agentview.Class]classify.Class{
	agentview.ClassUnknown: classify.Unknown, agentview.ClassMachine: classify.Machine, agentview.ClassHuman: classify.Human,
}

func (a *API) listMessages(ctx context.Context, v *auth.Viewer, p MessagesParams) (MessagePage, error) {
	q := store.MessageQuery{Limit: p.Limit, Before: (*store.MessageCursor)(p.Cursor)}
	q.Downgraded = downgrades[p.Downgraded]
	q.Label, q.Archived = p.Label, p.Archived
	var err error
	if q.Answer, err = a.answerFilter(p.Triage); err != nil {
		return MessagePage{}, err
	}
	if p.Prefix != nil {
		q.Prefix, q.Zone = p.Prefix.Prefix, p.Prefix.Zone
	}
	for _, c := range p.Class {
		q.Classes = append(q.Classes, classes[c])
	}
	ms, err := a.db.ListMessages(ctx, v, q)
	if err != nil {
		return MessagePage{}, err
	}
	page := MessagePage{Messages: make([]MessageSummary, len(ms))}
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
	}
	tops, err := a.tops(ctx, v, ids)
	if err != nil {
		return MessagePage{}, err
	}
	for i, m := range ms {
		page.Messages[i] = summary(v, m)
		page.Messages[i].Triage = tops[m.ID]
	}
	if len(ms) == p.Limit {
		last := ms[len(ms)-1]
		page.NextCursor = Cursor{ReceivedAt: last.ReceivedAt, ID: last.ID}.String()
	}
	return page, nil
}

func (a *API) message(ctx context.Context, v *auth.Viewer, p IDParams) (Message, error) {
	d, err := a.db.MessageByID(ctx, v, p.ID)
	if err != nil {
		return Message{}, err
	}
	// The recipients with their names come from the raw, like the
	// structured view: the messages columns keep bare addresses only
	// (§4). d.Raw is a raw this viewer may read (§13.8), so a viewer who
	// cannot read the message never gets here, and one who reads it only
	// through its sent copy gets the sent copy's header.
	raw, err := a.raw.ReadRaw(ctx, d.Raw)
	if err != nil {
		return Message{}, err
	}
	rc := mimeparse.ParseRecipients(raw)
	notes, err := a.db.Notes(ctx, v, d.ID)
	if err != nil {
		return Message{}, err
	}
	sc, err := a.latestScore(ctx, v, d.ID)
	if err != nil {
		return Message{}, err
	}
	out := Message{
		MessageSummary: summary(v, d.MessageSummary),
		Extractions:    make([]agentview.Extraction, len(d.Extractions)),
		MessageID:      d.MessageID, InReplyTo: d.InReplyTo, References: nonNil(d.References),
		Date: d.Date, BodyText: d.BodyText, ParseError: d.ParseError,
		To: addressesOf(rc.To), Cc: addressesOf(rc.Cc), ReplyTo: addressesOf(rc.ReplyTo), Reply: replyAddressing(d, rc),
		Notes: notesOut(notes), TriageScore: triageOut(v, sc),
	}
	out.Triage = topsOf(sc)
	for i, x := range d.Extractions {
		out.Extractions[i] = agentview.ExtractionOf(x)
	}
	// Envelope and authentication facts are delivery data (§13.4).
	if v.Has(auth.DeliveryRead) {
		out.Deliveries = make([]Delivery, len(d.Deliveries))
		for i, f := range d.Deliveries {
			out.Deliveries[i] = Delivery{
				ID: f.ID, Zone: f.Zone, Prefix: f.Prefix, PrefixClaimed: f.PrefixState == store.PrefixClaimed,
				EnvelopeFrom: f.EnvelopeFrom, EnvelopeTo: f.EnvelopeTo, ReceivedAt: f.ReceivedAt,
				Auth: authOf(f.Auth),
			}
			if f.Leak.Verdict != leak.None {
				out.Deliveries[i].Leak = &agentview.LeakCheck{
					To: f.Prefix + "@" + f.Zone, Verdict: agentview.LeakVerdict(f.Leak.Verdict), Domain: f.Leak.Domain,
				}
			}
		}
	}
	return out, nil
}

// messageView is the structured view of §8.1 that the reading pane
// renders (§12.1), the same tree get_message gives agents. Delivery facts
// in it need delivery:read, as above.
func (a *API) messageView(ctx context.Context, v *auth.Viewer, p ViewParams) (agentview.View, error) {
	d, err := a.db.MessageByID(ctx, v, p.ID)
	if err != nil {
		return agentview.View{}, err
	}
	raw, err := a.raw.ReadRaw(ctx, d.Raw)
	if err != nil {
		return agentview.View{}, err
	}
	sc, err := a.latestScore(ctx, v, d.ID)
	if err != nil {
		return agentview.View{}, err
	}
	in := agentview.InputOf(v, d, raw)
	in.Triage = agentview.TriageOf(sc)
	view, err := agentview.Select(agentview.Build(in), p.Level, p.Anchor)
	if errors.Is(err, agentview.ErrNoAnchor) {
		return view, openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "anchor %s is not in this message", p.Anchor)
	}
	return view, err
}

// replyAddressing addresses a reply to d, whose header recipients are
// rc. Ours are the addresses d reached us at and, for mail we sent, its
// From: a reply to all leaves them out, as it leaves out the
// recipients already in To. Ours is no wider than that: another address
// at one of our zones may be another person's (§13).
func replyAddressing(d store.MessageDetail, rc mimeparse.Recipients) ReplyAddressing {
	ours := map[string]bool{}
	for _, a := range d.Recipients {
		ours[store.FoldAddr(a)] = true
	}
	if d.Outgoing != nil {
		ours[store.FoldAddr(d.Outgoing.From)] = true
	}
	out := ReplyAddressing{To: []agentview.Address{}, Cc: []agentview.Address{}, UnverifiedReplyTo: []string{}}
	seen := map[string]bool{}
	add := func(to *[]agentview.Address, name, a string) {
		if f := store.FoldAddr(a); a != "" && !seen[f] {
			seen[f] = true
			*to = append(*to, agentview.Address{Name: name, Addr: a})
		}
	}
	switch {
	case d.Outgoing != nil:
		// Our own mail: a follow-up goes where it went, and its Reply-To
		// never counts.
		for _, m := range rc.To {
			if !ours[store.FoldAddr(m.Addr)] {
				add(&out.To, m.Name, m.Addr)
			}
		}
		if len(out.To) == 0 {
			add(&out.To, d.FromName, d.FromAddr)
		}
	case d.ReplyToCounts && len(rc.ReplyTo) > 0:
		for _, m := range rc.ReplyTo {
			add(&out.To, m.Name, m.Addr)
		}
	default:
		add(&out.To, d.FromName, d.FromAddr)
		named := map[string]bool{store.FoldAddr(d.FromAddr): true}
		for _, m := range rc.ReplyTo {
			if f := store.FoldAddr(m.Addr); !named[f] {
				named[f] = true
				out.UnverifiedReplyTo = append(out.UnverifiedReplyTo, m.Addr)
			}
		}
	}
	for _, m := range slices.Concat(rc.To, rc.Cc) {
		if !ours[store.FoldAddr(m.Addr)] {
			add(&out.Cc, m.Name, m.Addr)
		}
	}
	return out
}

func addressesOf(ms []mimeparse.Mailbox) []agentview.Address {
	out := make([]agentview.Address, len(ms))
	for i, m := range ms {
		out[i] = agentview.Address{Name: m.Name, Addr: m.Addr}
	}
	return out
}

func authOf(a *authres.Auth) *DeliveryAuth {
	if a == nil {
		return nil
	}
	out := &DeliveryAuth{Trusted: a.Trusted, Reason: a.Reason, AuthservID: a.AuthservID, DMARC: a.DMARC, RemoteIP: a.RemoteIP}
	for _, res := range a.Results {
		out.Results = append(out.Results, DeliveryAuthResult{Method: res.Method, Result: res.Result, Props: res.Props})
	}
	return out
}

func (a *API) thread(ctx context.Context, v *auth.Viewer, p IDParams) (Thread, error) {
	t, err := a.db.ThreadByID(ctx, v, p.ID)
	if err != nil {
		return Thread{}, err
	}
	out := Thread{
		ID: t.ID, Subject: t.Subject, Participants: nonNil(t.Participants),
		FirstAt: t.FirstAt, LastAt: t.LastAt, Messages: make([]MessageSummary, len(t.Messages)),
	}
	ids := make([]string, len(t.Messages))
	for i, m := range t.Messages {
		ids[i] = m.ID
	}
	tops, err := a.tops(ctx, v, ids)
	if err != nil {
		return Thread{}, err
	}
	for i, m := range t.Messages {
		out.Messages[i] = summary(v, m)
		out.Messages[i].Triage = tops[m.ID]
	}
	return out, nil
}

func (a *API) prefixes(ctx context.Context, v *auth.Viewer, p PrefixesParams) (PrefixList, error) {
	q := store.PrefixQuery{Limit: p.Limit, Offset: p.Offset}
	q.State = map[PrefixState]int{"claimed": store.PrefixClaimed, "discovered": store.PrefixDiscovered}[p.State]
	sum, err := a.db.PrefixStats(ctx, v, q)
	if err != nil {
		return PrefixList{}, err
	}
	out := PrefixList{Messages: sum.Messages, Downgraded: sum.Downgraded, Total: sum.Total, Prefixes: make([]Prefix, len(sum.Prefixes))}
	for i, p := range sum.Prefixes {
		out.Prefixes[i] = prefixOut(p)
	}
	return out, nil
}

// offenseLimit is how many offending messages the prefix detail lists:
// enough to see a pattern, the rest is the prefix's message list.
const offenseLimit = 20

func (a *API) prefix(ctx context.Context, v *auth.Viewer, p PrefixParams) (PrefixDetail, error) {
	d, err := a.db.PrefixByAddress(ctx, v, p.Address.Zone, p.Address.Prefix, offenseLimit)
	if err != nil {
		return PrefixDetail{}, err
	}
	out := PrefixDetail{Prefix: prefixOut(d.PrefixStat), Offenses: make([]Offense, len(d.Offenses))}
	for i, o := range d.Offenses {
		out.Offenses[i] = Offense{Message: summary(v, o.Message), Verdict: agentview.LeakVerdict(o.Verdict), Domain: o.Domain, DeliveredAt: o.DeliveredAt}
	}
	return out, nil
}

func prefixOut(p store.PrefixStat) Prefix {
	out := Prefix{
		Zone: p.Zone, Prefix: p.Prefix, Address: p.Prefix + "@" + p.Zone,
		Claimed: p.State == store.PrefixClaimed, Label: p.Label,
		FirstSeen: p.FirstSeen, LastSeen: p.LastSeen, Messages: p.Messages, Downgraded: p.Downgraded,
		ExpectedDomains: nonNil(p.ExpectedDomains), Observed: make([]ObservedDomain, len(p.Observed)),
		Leaks: p.Leaks, Unverified: p.Unverified, Spoofs: p.Spoofs, LastOffense: p.LastOffense,
	}
	for i, o := range p.Observed {
		out.Observed[i] = ObservedDomain{Domain: o.Domain, Messages: o.Messages, LastSeen: o.LastSeen, Expected: o.Expected}
	}
	return out
}

func (a *API) search(ctx context.Context, v *auth.Viewer, p SearchParams) (SearchResult, error) {
	env := search.Env{Now: time.Now()}
	if p.TZ != nil {
		env.Loc = p.TZ.Location
	}
	if a.triage != nil {
		env.Triage = triage.ParseFilter
	}
	q, err := search.Parse(p.Q, env)
	if err != nil {
		return SearchResult{}, err
	}
	res, err := a.db.Search(ctx, v, store.SearchQuery{
		Filter: q.Filter, Limit: p.Limit, Before: (*store.MessageCursor)(p.Cursor),
		IncludeDowngraded: p.Downgraded == "include", Triage: a.triage,
	})
	if err != nil {
		return SearchResult{}, err
	}
	ids := make([]string, len(res.Hits))
	for i, h := range res.Hits {
		ids[i] = h.ID
	}
	tops, err := a.tops(ctx, v, ids)
	if err != nil {
		return SearchResult{}, err
	}
	out := SearchResult{Query: q.Normalized, Hints: make([]SearchHint, len(q.Hints)), Total: res.Total, Hits: make([]MessageSummary, len(res.Hits))}
	for i, h := range q.Hints {
		out.Hints[i] = SearchHint{Term: h.Term, Suggest: h.Suggest}
	}
	for i, h := range res.Hits {
		out.Hits[i] = MessageSummary{
			ID: h.ID, ThreadID: h.ThreadID, ReceivedAt: h.ReceivedAt, Subject: h.Subject,
			FromAddr: h.FromAddr, FromName: h.FromName, Snippet: h.Snippet, Class: classOf(h.Class),
			Downgraded: h.Downgraded, Quarantined: h.Quarantined, Recipients: []string{},
			Outgoing: outgoingOut(h.Outgoing), Labels: nonNil(h.Labels), Archived: h.Archived, Triage: tops[h.ID],
		}
		// A hit from the notification zone shows its code there too
		// (§12.9); other mail is found by its words, not its numbers.
		if h.Class == classify.Machine {
			out.Hits[i].Extracted = extracted(h.Extracted, time.Now())
			if v.Has(auth.DeliveryRead) {
				out.Hits[i].SenderVerified = h.SenderVerified
			}
		}
	}
	if len(res.Hits) == p.Limit {
		last := res.Hits[len(res.Hits)-1]
		out.NextCursor = Cursor{ReceivedAt: last.ReceivedAt, ID: last.ID}.String()
	}
	return out, nil
}

func summary(v *auth.Viewer, m store.MessageSummary) MessageSummary {
	out := MessageSummary{
		ID: m.ID, ThreadID: m.ThreadID, ReceivedAt: m.ReceivedAt, Subject: m.Subject,
		FromAddr: m.FromAddr, FromName: m.FromName, Snippet: m.Snippet, Class: classOf(m.Class),
		Downgraded: m.Downgraded, Quarantined: m.Quarantined, HasAttach: m.HasAttach,
		Recipients: nonNil(m.Recipients), Outgoing: outgoingOut(m.Outgoing),
		Extracted: extracted(m.Extracted, time.Now()), Labels: nonNil(m.Labels), Archived: m.Archived,
	}
	if v.Has(auth.DeliveryRead) {
		out.SenderVerified = m.SenderVerified
	}
	return out
}

// extracted is the summary's Extracted from the best extraction of each
// kind, as the store lists them; nil when there is no code or link.
func extracted(xs []store.Extraction, now time.Time) *Extracted {
	var out Extracted
	for _, x := range xs {
		e := agentview.ExtractionOf(x)
		switch e.Kind {
		case "otp":
			out.Code = &e
		case "link":
			out.Link = &e
		case "expiry":
			out.ExpiresAt = e.ExpiresAt
		}
	}
	if out.Code == nil && out.Link == nil {
		return nil
	}
	out.Expired = out.ExpiresAt != nil && !now.Before(*out.ExpiresAt)
	return &out
}

func classOf(c classify.Class) agentview.Class { return agentview.Class(c.String()) }

// nonNil keeps JSON arrays arrays: the schema promises no null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
