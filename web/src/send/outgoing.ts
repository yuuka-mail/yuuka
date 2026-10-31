import { ApiError } from "@/api/client";
import type { Actor, Code, DraftBody, HoldReason, Message, OutboxEntry, OutboxState, RecipientState, SendingIdentity, SessionInfo, Tier } from "@/api/types";
import { fullTime, sender } from "@/lib/format";

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

const outboxStates: Record<OutboxState, StateInfo> = {
  held: { label: "held for review", tone: "moving" },
  queued: { label: "queued", tone: "moving" },
  sending: { label: "sending", tone: "moving" },
  sent: { label: "sent", tone: "ok" },
  // Some recipients took it, some are still being retried or refused.
  partial: { label: "partly sent", tone: "warn" },
  // §4.5: past the commit point with no answer. The words are the ones
  // §4.5 asks for: the person decides from here.
  unknown: { label: "may have been sent", tone: "warn" },
  failed: { label: "failed, will retry", tone: "warn" },
  dead: { label: "not sent", tone: "bad" },
  withdrawn: { label: "withdrawn", tone: "plain" },
};

export function outboxState(s: OutboxState | string): StateInfo {
  return outboxStates[s as OutboxState] ?? { label: s, tone: "plain" };
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

const recipientStates: Record<RecipientState, StateInfo> = {
  pending: { label: "pending", tone: "moving" },
  accepted: { label: "accepted", tone: "ok" },
  deferred: { label: "deferred", tone: "warn" },
  rejected: { label: "rejected", tone: "bad" },
  unknown: { label: "unknown", tone: "warn" },
};

export function recipientState(s: RecipientState): StateInfo {
  return recipientStates[s] ?? { label: s, tone: "plain" };
}

/**
 * The outbox filters of the outbox view. Attention is what a person
 * should look at; unknown among it is what only a person can settle.
 */
export const outboxFilters = {
  all: { label: "All", states: undefined },
  attention: { label: "Needs attention", states: ["unknown", "partial", "failed", "dead"] },
  moving: { label: "In flight", states: ["held", "queued", "sending"] },
  sent: { label: "Sent", states: ["sent"] },
} as const satisfies Record<string, { label: string; states: readonly OutboxState[] | undefined }>;
export type OutboxFilter = keyof typeof outboxFilters;

const holdReasons: Record<HoldReason, string> = {
  policy: "drafts only",
  owner: "owner cap",
  limit: "over the sending limit",
  recipients: "recipient outside the grant",
  suspended: "grant suspended",
  loop: "mail loop guard",
  review: "review not available yet",
  withdrawn: "withdrawn",
};

/** A few words for why a submission stopped at a draft (§8.9). */
export function holdLabel(r: HoldReason | undefined): string {
  return r ? (holdReasons[r] ?? r) : "held";
}

const tiers: Record<Tier, string> = { none: "no use", draft: "drafts only", review: "review", auto: "auto" };
export const tierLabel = (t: Tier) => tiers[t] ?? t;

const scopes = { thread: "the thread's participants", allowlist: "allowlisted recipients", any: "any recipient" } as const;

/**
 * What pressing Send as identity i would do, before anything is
 * submitted. sends is a preview: recipients and limits are checked on
 * submission, so a "queues" can still come back held, never the reverse.
 */
export function standing(i: SendingIdentity): { sends: boolean; headline: string; detail: string } {
  if (!i.sends) return { sends: false, headline: `Held as a draft: ${holdLabel(i.hold_reason)}`, detail: i.hold_detail ?? "" };
  const limits = [i.limits.max_per_hour && `${i.limits.max_per_hour}/hour`, i.limits.max_per_day && `${i.limits.max_per_day}/day`].filter(Boolean);
  const scope = i.recipient_scope === "allowlist" && i.allowlist.length ? `${scopes.allowlist} (${i.allowlist.join(", ")})` : scopes[i.recipient_scope];
  return {
    sends: true,
    headline: "Send queues it",
    detail: [`${tierLabel(i.tier)}`, `to ${scope}`, ...limits].join(" · "),
  };
}

/** The principal as `kind:name`, the form the session names it in. */
export const principal = (a: Actor) => `${a.kind}:${a.name}`;

export function isMine(a: Actor, s: SessionInfo) {
  return principal(a) === s.principal;
}

export function isHuman(s: SessionInfo) {
  return s.principal.startsWith("human:");
}

/** What the session may do on the sending side; each needs its scope (§13.3). */
export function abilities(s: SessionInfo) {
  const has = (x: SessionInfo["scopes"][number]) => s.scopes.includes(x) || s.scopes.includes("admin");
  return {
    compose: has("draft:create"),
    // The outbox lists Bcc recipients, which are delivery facts.
    outbox: has("delivery:read"),
    // §4.5: only a person settles unknown delivery, and resend sends.
    resolve: isHuman(s) && has("submission:send"),
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
export function draftBody(f: { from: string; to: string; cc: string; bcc: string; subject: string; body: string; inReplyTo?: string }): DraftBody {
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

const problems: Partial<Record<Code, string>> = {
  invalid_body: "Something in the draft is not valid",
  invalid_param: "The request was not valid",
  no_identity: "No identity covers this From address",
  conflict: "Already submitted: the draft has gone to the outbox",
  not_found: "Not found: it may have been deleted",
  scope_required: "This session is not allowed to do that",
  forbidden: "Not allowed",
  submission_rejected: "yuuka refused the submission",
  rate_limited: "Too many requests; try again in a moment",
  unavailable: "yuuka is busy; try again in a moment",
  unauthenticated: "The session has ended; sign in again",
  body_too_large: "The draft is too large",
};

// The codes whose detail names what is wrong (a field, an address).
const specific = new Set<Code>(["invalid_body", "invalid_param", "no_identity", "submission_rejected"]);

/**
 * An error as people read it: a sentence chosen by its stable code, and
 * the server's detail only where the code says it is specific.
 */
export function problem(err: unknown): { text: string; detail?: string; code?: string } {
  if (err instanceof ApiError) {
    const text = (err.code && problems[err.code]) || `Request failed (${err.status})`;
    return { text, detail: err.code && specific.has(err.code) ? err.message : undefined, code: err.code };
  }
  return { text: "Could not reach yuuka" };
}
