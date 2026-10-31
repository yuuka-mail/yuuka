import { ApiError } from "@/api/client";
import type {
  Actor,
  CheckName,
  Code,
  Draft,
  DraftBody,
  HoldReason,
  Message,
  OutboxEntry,
  OutboxState,
  RecipientScope,
  RecipientState,
  SendingIdentity,
  SessionInfo,
  Tier,
} from "@/api/types";
import { fullTime, sender } from "@/lib/format";
import { m as msg } from "@/paraglide/messages.js";

// What yuuka says about outgoing mail, as words and tones. Everything
// here is the system speaking (§12.5): the outbox state, why a draft was
// held, what an identity would do with a submission. Pure, so the
// wording is tested and the components only lay it out.

/**
 * ok is done, moving is on its way and needs nothing, warn wants a look
 * or a decision, bad did not happen, plain is neither.
 */
export type Tone = "ok" | "moving" | "warn" | "bad" | "plain";

export interface StateInfo {
  label: string;
  tone: Tone;
}

// Functions, not strings: the words are the current locale's (§12.11).
const outboxStates: Record<OutboxState, () => StateInfo> = {
  held: () => ({ label: msg.send_state_held(), tone: "moving" }),
  queued: () => ({ label: msg.send_state_queued(), tone: "moving" }),
  sending: () => ({ label: msg.send_state_sending(), tone: "moving" }),
  sent: () => ({ label: msg.send_state_sent(), tone: "ok" }),
  // Some recipients took it, some are still being retried or refused.
  partial: () => ({ label: msg.send_state_partial(), tone: "warn" }),
  // §4.5: past the commit point with no answer. The words are the ones
  // §4.5 asks for: the person decides from here.
  unknown: () => ({ label: msg.send_state_unknown(), tone: "warn" }),
  failed: () => ({ label: msg.send_state_failed(), tone: "warn" }),
  dead: () => ({ label: msg.send_state_dead(), tone: "bad" }),
  withdrawn: () => ({ label: msg.send_state_withdrawn(), tone: "plain" }),
};

export function outboxState(s: OutboxState | string): StateInfo {
  return outboxStates[s as OutboxState]?.() ?? { label: s, tone: "plain" };
}

/** Whether a row changes without anyone deciding, so it is worth polling. */
export function outboxMoves(e: Pick<OutboxEntry, "state" | "next_retry_at">): boolean {
  switch (e.state) {
    case "held":
    case "queued":
    case "sending":
    case "failed":
      return true;
    case "partial":
      return !!e.next_retry_at;
    default:
      return false;
  }
}

const recipientStates: Record<RecipientState, () => StateInfo> = {
  pending: () => ({ label: msg.send_rcpt_pending(), tone: "moving" }),
  accepted: () => ({ label: msg.send_rcpt_accepted(), tone: "ok" }),
  deferred: () => ({ label: msg.send_rcpt_deferred(), tone: "warn" }),
  rejected: () => ({ label: msg.send_rcpt_rejected(), tone: "bad" }),
  unknown: () => ({ label: msg.send_rcpt_unknown(), tone: "warn" }),
};

export function recipientState(s: RecipientState): StateInfo {
  return recipientStates[s]?.() ?? { label: s, tone: "plain" };
}

/**
 * The outbox filters of the outbox view. Attention is what a person
 * should look at; unknown among it is what only a person can settle.
 */
export const outboxFilters = {
  all: {
    get label() {
      return msg.send_filter_all();
    },
    states: undefined,
  },
  // Held for the recall window (§8.9): the one moment a person can
  // still stop mail the review let through.
  held: {
    get label() {
      return msg.send_filter_held();
    },
    states: ["held"],
  },
  attention: {
    get label() {
      return msg.send_filter_attention();
    },
    states: ["unknown", "partial", "failed", "dead"],
  },
  moving: {
    get label() {
      return msg.send_filter_moving();
    },
    states: ["held", "queued", "sending"],
  },
  sent: {
    get label() {
      return msg.send_filter_sent();
    },
    states: ["sent"],
  },
} as const satisfies Record<
  string,
  { readonly label: string; states: readonly OutboxState[] | undefined }
>;
export type OutboxFilter = keyof typeof outboxFilters;

const holdReasons: Record<HoldReason, () => string> = {
  policy: () => msg.send_hold_policy(),
  owner: () => msg.send_hold_owner(),
  limit: () => msg.send_hold_limit(),
  recipients: () => msg.send_hold_recipients(),
  suspended: () => msg.send_hold_suspended(),
  loop: () => msg.send_hold_loop(),
  review: () => msg.send_hold_review(),
  withdrawn: () => msg.send_hold_withdrawn(),
};

/** A few words for why a submission stopped at a draft (§8.9). */
export function holdLabel(r: HoldReason | undefined): string {
  return r ? (holdReasons[r]?.() ?? r) : msg.send_hold_unknown();
}

const tiers: Record<Tier, () => string> = {
  none: () => msg.send_tier_none(),
  draft: () => msg.send_tier_draft(),
  review: () => msg.send_tier_review(),
  auto: () => msg.send_tier_auto(),
};
export const tierLabel = (t: Tier) => tiers[t]?.() ?? t;

const scopes = {
  thread: () => msg.send_scope_thread(),
  allowlist: () => msg.send_scope_allowlist(),
  any: () => msg.send_scope_any(),
} as const;

/** Whom recipient_scope s lets mail go to, with the allowlist when it has one. */
export function scopeText(s: RecipientScope, allowlist: string[]): string {
  return s === "allowlist" && allowlist.length
    ? msg.send_scope_allowlist_some({ list: allowlist.join(", ") })
    : (scopes[s]?.() ?? s);
}

/** A recall window as people say it: "10 min", "2 h", "45 s". */
export function duration(seconds: number): string {
  if (seconds < 60) return msg.duration_s({ n: seconds });
  if (seconds % 3600 === 0) return msg.duration_h({ n: seconds / 3600 });
  return msg.duration_min({ n: Math.round(seconds / 60) });
}

/** Whether a submission as i goes to the review's checks: the server gives the window only then. */
export function goesToReview(i: Pick<SendingIdentity, "hold_reason" | "hold_seconds">): boolean {
  return i.hold_reason === "review" && i.hold_seconds !== undefined;
}

/** What the From picker says beside identity i: "sends (auto)", "review, 10 min to recall", "held: drafts only". */
export function identityNote(i: SendingIdentity): string {
  if (goesToReview(i))
    return msg.send_opt_review({ window: duration(i.hold_seconds ?? defaultHoldSeconds) });
  return i.sends
    ? msg.send_opt_sends({ tier: i.tier })
    : msg.send_note_held({ reason: holdLabel(i.hold_reason) });
}

/** sendpolicy.DefaultHoldSeconds: the window when a grant leaves it unset. */
export const defaultHoldSeconds = 600;

/** The review tier with its window: "review — held 10 min after the checks". */
export function reviewText(holdSeconds: number | undefined): string {
  const s = holdSeconds ?? defaultHoldSeconds;
  return s > 0 ? msg.send_tier_review_hold({ window: duration(s) }) : msg.send_tier_review_now();
}

/**
 * What pressing Send as identity i would do, before anything is
 * submitted. sends is a preview: recipients and limits are checked on
 * submission, so a "queues" can still come back held, never the reverse.
 * On the review tier it goes to the checks, and from them to the recall
 * window or back to a person (§8.9): the server says so with hold_reason
 * review and the window in hold_seconds, and sends false, since nothing
 * goes out on the press itself. Here that counts as sending.
 */
export function standing(i: SendingIdentity): { sends: boolean; headline: string; detail: string } {
  const review = goesToReview(i);
  if (!i.sends && !review)
    return {
      sends: false,
      headline: msg.send_held_as_draft({ reason: holdLabel(i.hold_reason) }),
      detail: i.hold_detail ?? "",
    };
  const limits = [
    i.limits.max_per_hour && msg.send_limit_hour({ n: i.limits.max_per_hour }),
    i.limits.max_per_day && msg.send_limit_day({ n: i.limits.max_per_day }),
  ].filter(Boolean);
  return {
    sends: true,
    headline: review ? msg.send_reviews_it() : msg.send_queues_it(),
    detail: [
      review ? reviewText(i.hold_seconds) : tierLabel(i.tier),
      msg.send_to_scope({ scope: scopeText(i.recipient_scope, i.allowlist) }),
      ...limits,
    ].join(" · "),
  };
}

const checks: Record<CheckName, () => string> = {
  first_contact: () => msg.review_check_first_contact(),
  extracted_values: () => msg.review_check_extracted(),
  cross_thread: () => msg.review_check_cross_thread(),
  link_domains: () => msg.review_check_links(),
  readable: () => msg.review_check_readable(),
};

/** The name of a check of the review tier (§8.9). */
export const checkLabel = (c: CheckName) => checks[c]?.() ?? c;

/**
 * s quoted for a POSIX shell where it needs it, as the server quotes the
 * commands it names (*@zone does): a command shown is one to paste.
 */
export function shellWord(s: string): string {
  if (/^[A-Za-z0-9@._+:,=/-]+$/.test(s)) return s;
  return `'${s.replaceAll("'", `'\\''`)}'`;
}

/** The host command that resumes a suspended grant (§8.9). */
export function resumeCommand(identity: string, principal: string): string {
  return `yuuka grant set --identity ${shellWord(identity)} --principal ${shellWord(principal)} --resume`;
}

/** The principal as `kind:name`, the form the session names it in. */
export const principal = (a: Actor) => `${a.kind}:${a.name}`;

export function isMine(a: Actor, s: SessionInfo) {
  return principal(a) === s.principal;
}

/**
 * The one person who may confirm draft d when that is not the viewer:
 * the owner of a person's identity (§8.9 owner cap). The draft names the
 * owner itself, since the viewer may have no tier on the identity and
 * /identities then leaves it out.
 */
export function onlyConfirmer(
  d: Pick<Draft, "identity_owner">,
  s: SessionInfo,
): string | undefined {
  const o = d.identity_owner;
  return o.kind === "human" && !isMine(o, s) ? o.name : undefined;
}

/**
 * What Send would do as an address whose identity the viewer has no
 * tier on, so /identities leaves it out and only the draft names its
 * owner. A person's address is theirs alone (§8.9 owner cap); anyone
 * else's needs a grant. Either way the submission stops at the draft.
 */
export function strangerStanding(
  owner: Actor,
  s: SessionInfo,
): { reason: HoldReason; note: string; headline: string; detail: string } {
  if (owner.kind === "human" && !isMine(owner, s)) {
    return {
      reason: "owner",
      note: msg.send_note_held({ reason: holdLabel("owner") }),
      headline: msg.send_only_owner_sends({ name: owner.name }),
      detail: msg.send_only_owner_sends_detail({ name: owner.name }),
    };
  }
  return {
    reason: "policy",
    note: msg.send_note_no_grant(),
    headline: msg.send_held_no_grant(),
    detail: isMine(owner, s)
      ? msg.send_no_grant_yours()
      : msg.send_no_grant_theirs({ owner: principal(owner) }),
  };
}

/** Whether the session may recall held row e: a person any, others their own (§8.9). */
export function recallable(
  e: Pick<OutboxEntry, "state" | "submitted_by">,
  s: SessionInfo,
  can: { recall: boolean },
): boolean {
  return e.state === "held" && can.recall && (isHuman(s) || isMine(e.submitted_by, s));
}

export function isHuman(s: SessionInfo) {
  return s.principal.startsWith("human:");
}

/**
 * What the session may do; each needs its scope (§13.3), and some only a
 * person may do whatever the scope says (§8.5, §4.5). The server checks
 * all of it again: this only decides what is offered.
 */
export function abilities(s: SessionInfo) {
  const has = (x: SessionInfo["scopes"][number]) =>
    s.scopes.includes(x) || s.scopes.includes("admin");
  return {
    compose: has("draft:create"),
    // The outbox lists Bcc recipients, which are delivery facts.
    outbox: has("delivery:read"),
    // §4.5: only a person settles unknown delivery, and resend sends.
    resolve: isHuman(s) && has("submission:send"),
    // Labels, archive and class (§8.5).
    annotate: has("message:annotate"),
    // Deciding a proposal is a person's; creating a rule with it needs rules too.
    decide: isHuman(s) && has("message:annotate"),
    // Rules are a standing capability: a person's, with rule:manage.
    rules: isHuman(s) && has("rule:manage") && has("message:read"),
    // Held mail (§8.9): a person may recall any they can see; anyone
    // else only their own, which recallable checks per row.
    recall: has("submission:send"),
    // Claiming a prefix and its expected domains follow the mail (§12.12).
    prefixes: has("prefix:manage") && has("prefix:read"),
    // Who may do what (§13.10): a person's own session, with admin. A
    // token never, whatever its scopes; the server checks it again.
    authority: isHuman(s) && s.credential_kind === "session" && s.scopes.includes("admin"),
    // Anyone with the scope writes their own note; a person deletes anyone's (§8.10).
    notes: has("message:annotate"),
    deleteNotes: isHuman(s) && has("message:annotate"),
    // The scorer's report is a person's (§15.4); the server checks it again.
    triage: isHuman(s) && has("message:read"),
  };
}
export type Abilities = ReturnType<typeof abilities>;

/** Addresses as typed: separated by commas, semicolons or spaces. */
export function parseAddrs(s: string): string[] {
  return s
    .split(/[\s,;]+/)
    .map((a) => a.trim())
    .filter(Boolean);
}

/** The typed addresses that are not one bare address each. */
export function badAddrs(s: string): string[] {
  return parseAddrs(s).filter((a) => !/^[^@\s<>"]+@[^@\s<>"]+$/.test(a));
}

export function replySubject(s: string): string {
  return /^re:/i.test(s.trim()) ? s.trim() : `Re: ${s.trim()}`;
}

/**
 * The body of a reply to m: room to write, then m's text quoted with >,
 * as mail has always done it. The quote is text in a textarea, never
 * markup; Markdown renders it as a blockquote on the way out (§10.2.1).
 */
export function replyBody(m: Message): string {
  const text = m.body_text.replace(/\s+$/, "");
  if (!text) return "";
  const when = fullTime(m.date ?? m.received_at);
  const quoted = text
    .split(/\r?\n/)
    .map((l) => (l ? `> ${l}` : ">"))
    .join("\n");
  return `\n\nOn ${when}, ${sender(m.from_name, m.from_addr)} wrote:\n${quoted}\n`;
}

/** The draft body to save, from the composer's fields. */
export function draftBody(f: {
  from: string;
  to: string;
  cc: string;
  bcc: string;
  subject: string;
  body: string;
  inReplyTo?: string;
}): DraftBody {
  return {
    from: f.from || undefined,
    to: parseAddrs(f.to),
    cc: parseAddrs(f.cc),
    bcc: parseAddrs(f.bcc),
    subject: f.subject,
    body_markdown: f.body,
    in_reply_to: f.inReplyTo || undefined,
  };
}

/** A line of a draft's body for a list row: the first that is not quoted. */
export function draftSnippet(md: string): string {
  for (const l of md.split(/\r?\n/)) {
    const t = l.trim();
    if (t && !t.startsWith(">")) return t;
  }
  return "";
}

const problems: Partial<Record<Code, () => string>> = {
  invalid_body: () => msg.send_problem_invalid_body(),
  invalid_param: () => msg.send_problem_invalid_param(),
  no_identity: () => msg.send_problem_no_identity(),
  conflict: () => msg.send_problem_conflict(),
  not_found: () => msg.send_problem_not_found(),
  scope_required: () => msg.send_problem_scope_required(),
  forbidden: () => msg.send_problem_forbidden(),
  submission_rejected: () => msg.send_problem_submission_rejected(),
  rate_limited: () => msg.send_problem_rate_limited(),
  unavailable: () => msg.send_problem_unavailable(),
  unauthenticated: () => msg.send_problem_unauthenticated(),
  body_too_large: () => msg.send_problem_body_too_large(),
  invalid_rule: () => msg.problem_invalid_rule(),
  widening_requires_cli: () => msg.problem_widening(),
};

// The codes whose detail names what is wrong (a field, an address), or
// the host command that does it (widening_requires_cli).
const specific = new Set<Code>([
  "invalid_body",
  "invalid_param",
  "no_identity",
  "submission_rejected",
  "invalid_rule",
  "widening_requires_cli",
]);

/**
 * An error as people read it: a sentence chosen by its stable code, and
 * the server's detail only where the code says it is specific. A
 * caller can word a code for its own context: a conflict on a proposal
 * is not one on a draft.
 */
export function problem(
  err: unknown,
  words: Partial<Record<Code, () => string>> = {},
): { text: string; detail?: string; code?: string } {
  if (err instanceof ApiError) {
    const text =
      (err.code && (words[err.code] ?? problems[err.code])?.()) ||
      msg.send_problem_status({ status: String(err.status) });
    const detail = err.path ? `${err.path}: ${err.message}` : err.message;
    return {
      text,
      detail: err.code && specific.has(err.code) ? detail : undefined,
      code: err.code,
    };
  }
  return { text: msg.send_problem_unreachable() };
}
