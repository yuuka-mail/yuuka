import type { Auth, Facts, LeakCheck, LeakVerdict, Message, Sent, Stripped } from "@/api/types";

import { fullTime } from "@/lib/format";
import { m as msg } from "@/paraglide/messages.js";
import { outboxState } from "@/send/outgoing";

// What the system knows about a message, as opposed to what the message
// says about itself. These render in the system voice: they come from the
// trusted receiver, the prefix registry and the parser (the facts half of
// the structured view, §8.1), never from headers the sender wrote.

export type Tone = "ok" | "bad" | "warn" | "plain";
export interface Part {
  text: string;
  tone: Tone;
}
export interface Fact {
  label: string;
  parts: Part[];
}

const resultTone = (r: string): Tone =>
  r === "pass" ? "ok" : r === "fail" || r === "softfail" || r === "permerror" ? "bad" : "plain";

function authParts(a: Auth): Part[] {
  if (a.withheld) return [{ text: msg.facts_auth_withheld(), tone: "plain" }];
  // Our own bytes never passed a receiver: no verdict, and nothing
  // suspect about that either (§4.6).
  if (a.not_received) return [{ text: msg.facts_auth_not_received(), tone: "plain" }];
  // The reason is the server's words, a note after ours (§12.11).
  if (!a.verified)
    return [
      {
        text: a.reason
          ? msg.facts_auth_unverified_because({ reason: a.reason })
          : msg.facts_auth_unverified(),
        tone: "warn",
      },
    ];
  const parts: Part[] = [];
  for (const [name, r] of [
    ["dmarc", a.dmarc],
    ["spf", a.spf],
    ["dkim", a.dkim],
  ] as const) {
    if (r) parts.push({ text: `${name}=${r}`, tone: resultTone(r) });
  }
  // DMARC alone says whether this domain is genuine (§8.1).
  if (a.from_domain)
    parts.push({ text: msg.facts_auth_for({ domain: a.from_domain }), tone: "plain" });
  return parts;
}

const verdictTone: Record<LeakVerdict, Tone> = { ok: "ok", leak: "warn", spoof: "bad" };

/** A leak check's verdict in words (§3.3): "leak", "泄漏". */
export function verdictLabel(v: LeakVerdict): string {
  return { ok: msg.facts_verdict_ok, leak: msg.facts_verdict_leak, spoof: msg.facts_verdict_spoof }[
    v
  ]();
}

/** One delivery's leak check (§3.3), e.g. "github@example.com" "leak" "from example.org". */
export function leakParts(c: LeakCheck): Part[] {
  const about =
    c.verdict === "spoof"
      ? msg.facts_leak_claims({ domain: c.domain ?? "" })
      : c.domain
        ? msg.facts_leak_from({ domain: c.domain })
        : msg.facts_leak_unproven();
  return [
    { text: c.to, tone: "plain" },
    { text: verdictLabel(c.verdict), tone: verdictTone[c.verdict] },
    { text: about, tone: "plain" },
  ];
}

export function strippedText(s: Stripped): string {
  const out: string[] = [];
  const add = (n: number | undefined, text: (i: { count: number }) => string) =>
    n && out.push(text({ count: n }));
  add(s.tracking_pixels, msg.facts_stripped_pixels);
  add(s.hidden, msg.facts_stripped_hidden);
  add(s.images, msg.facts_stripped_images);
  add(s.unsafe_links, msg.facts_stripped_links);
  add(s.quoted_lines, msg.facts_stripped_quoted);
  return out.join(", ");
}

/** The rows of the facts box; f is undefined until the view has loaded. */
export function systemFacts(m: Message, f: Facts | undefined): Fact[] {
  const facts: Fact[] = [];
  if (m.parse_error)
    facts.push({ label: msg.facts_parse_error(), parts: [{ text: m.parse_error, tone: "bad" }] });
  if (f?.sent) facts.push({ label: msg.facts_sent(), parts: sentParts(f.sent) });
  if (f) facts.push({ label: msg.facts_auth(), parts: authParts(f.auth) });
  if (f?.leak_checks?.length)
    facts.push({ label: msg.facts_prefix(), parts: f.leak_checks.flatMap(leakParts) });
  facts.push({
    label: f?.auth.not_received ? msg.facts_submitted() : msg.facts_received(),
    parts: [{ text: fullTime(m.received_at), tone: "plain" }],
  });
  facts.push({ label: msg.facts_class(), parts: [{ text: m.class, tone: "plain" }] });
  const stripped = f && strippedText(f.stripped);
  if (stripped)
    facts.push({ label: msg.facts_left_out(), parts: [{ text: stripped, tone: "plain" }] });
  return facts;
}

export interface Warning {
  /** What it is about in a word or two: "dmarc=fail", "leak". */
  key: string;
  text: string;
  tone: "bad" | "warn";
}

/**
 * What to be careful of, said by the UI from the facts it is about, as
 * the view's warnings say it for agents (§8.1). The view's own sentences
 * are English for agents; they show only for a keyword this list does not
 * know yet, and then as the server wrote them (§12.11). A forged sender,
 * a spoof and a broken parse are errors; an unproven sender, a leaked
 * address and a downgrade are cautions.
 */
export function warnings(m: Message, f: Facts | undefined): Warning[] {
  if (!f) return [];
  const out: Warning[] = [];
  const a = f.auth;
  if (!a.withheld && !a.not_received) {
    if (!a.verified)
      out.push({
        key: msg.facts_warn_unverified(),
        text: msg.facts_warn_unverified_text(),
        tone: "warn",
      });
    else if (a.dmarc === "fail")
      out.push({ key: "dmarc=fail", text: msg.facts_warn_forged(), tone: "bad" });
  }
  for (const c of f.leak_checks ?? []) {
    if (c.verdict === "spoof")
      out.push({
        key: verdictLabel("spoof"),
        text: msg.facts_warn_spoof({ to: c.to, domain: c.domain ?? "" }),
        tone: "bad",
      });
    else if (c.verdict === "leak")
      out.push({
        key: verdictLabel("leak"),
        text: c.domain
          ? msg.facts_warn_leak({ to: c.to, domain: c.domain })
          : msg.facts_warn_leak_unproven({ to: c.to }),
        tone: "warn",
      });
  }
  if (f.downgraded)
    out.push({
      key: msg.facts_warn_downgraded(),
      text: msg.facts_warn_downgraded_text(),
      tone: "warn",
    });
  if (m.quarantined)
    out.push({
      key: msg.facts_warn_quarantined(),
      text: msg.facts_warn_quarantined_text(),
      tone: "bad",
    });
  for (const w of f.warnings ?? []) {
    const i = w.indexOf(": ");
    const key = i > 0 ? w.slice(0, i) : w;
    if (!saidAbove.has(key)) out.push({ key, text: i > 0 ? w.slice(i + 2) : "", tone: "warn" });
  }
  return out;
}

// The keywords of the view's warnings that warnings() says itself.
const saidAbove = new Set([
  "unverified",
  "dmarc=fail",
  "spoof",
  "leak",
  "downgraded",
  "quarantined",
]);

/**
 * Whether the facts say nothing to act on: the trusted receiver passed
 * DMARC and nothing is downgraded, quarantined or warned about. Such mail
 * gets one line of facts instead of the box (§12.9), so that the box still
 * gets read when it does appear.
 */
export function isQuiet(m: Message, f: Facts | undefined): boolean {
  if (
    !f ||
    m.parse_error ||
    m.quarantined ||
    m.downgraded ||
    f.downgraded ||
    warnings(m, f).length > 0
  )
    return false;
  // Mail we sent and nobody delivered back: quiet while its outbox state
  // asks nothing of anyone; unknown, failed and the like open the box.
  if (f.auth.not_received) return !!f.sent && sentQuiet(f.sent.state);
  return !f.auth.withheld && f.auth.verified && f.auth.dmarc === "pass";
}

const sentQuiet = (s: string) => ["held", "queued", "sending", "sent", "withdrawn"].includes(s);

const sentTone = (s: string): Tone =>
  s === "sent" ? "ok" : s === "dead" ? "bad" : sentQuiet(s) ? "plain" : "warn";

/** The outbox state of mail we sent, in words: "queued", "may have been sent". */
export function sentState(s: Sent): Part {
  return { text: outboxState(s.state).label, tone: sentTone(s.state) };
}

function sentParts(s: Sent): Part[] {
  return [{ text: msg.facts_sent_as({ from: s.from }), tone: "plain" }, sentState(s)];
}
