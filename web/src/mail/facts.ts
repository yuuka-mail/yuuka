import type { Auth, Facts, Message, Stripped } from "@/api/types";

import { fullTime } from "@/lib/format";

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

const resultTone = (r: string): Tone => (r === "pass" ? "ok" : r === "fail" || r === "softfail" || r === "permerror" ? "bad" : "plain");

function authParts(a: Auth): Part[] {
  if (a.withheld) return [{ text: "withheld: this session lacks delivery:read", tone: "plain" }];
  if (!a.verified) return [{ text: `unverified${a.reason ? ` (${a.reason})` : ""}`, tone: "warn" }];
  const parts: Part[] = [];
  for (const [name, r] of [
    ["dmarc", a.dmarc],
    ["spf", a.spf],
    ["dkim", a.dkim],
  ] as const) {
    if (r) parts.push({ text: `${name}=${r}`, tone: resultTone(r) });
  }
  // DMARC alone says whether this domain is genuine (§8.1).
  if (a.from_domain) parts.push({ text: `for ${a.from_domain}`, tone: "plain" });
  return parts;
}

export function strippedText(s: Stripped): string {
  const out: string[] = [];
  const add = (n: number | undefined, one: string, many: string) => n && out.push(`${n} ${n === 1 ? one : many}`);
  add(s.tracking_pixels, "tracking pixel", "tracking pixels");
  add(s.hidden, "hidden element", "hidden elements");
  add(s.images, "image without alt text", "images without alt text");
  add(s.unsafe_links, "unsafe link target", "unsafe link targets");
  add(s.quoted_lines, "quoted line", "quoted lines");
  return out.join(", ");
}

/** The rows of the facts box; f is undefined until the view has loaded. */
export function systemFacts(m: Message, f: Facts | undefined): Fact[] {
  const facts: Fact[] = [];
  if (m.parse_error) facts.push({ label: "Parse error", parts: [{ text: m.parse_error, tone: "bad" }] });
  if (f) facts.push({ label: "Auth", parts: authParts(f.auth) });
  facts.push({ label: "Received", parts: [{ text: fullTime(m.received_at), tone: "plain" }] });
  facts.push({ label: "Class", parts: [{ text: m.class, tone: "plain" }] });
  const stripped = f && strippedText(f.stripped);
  if (stripped) facts.push({ label: "Left out", parts: [{ text: stripped, tone: "plain" }] });
  return facts;
}

export interface Warning {
  /** The fixed keyword the view starts the warning with, e.g. dmarc=fail. */
  key: string;
  text: string;
  tone: "bad" | "warn";
}

/**
 * The view's warnings, split at their keyword. A forged sender and a
 * broken parse are errors; an unproven sender or a downgrade are
 * cautions.
 */
export function warnings(f: Facts | undefined): Warning[] {
  return (f?.warnings ?? []).map((w) => {
    const i = w.indexOf(": ");
    const key = i > 0 ? w.slice(0, i) : w;
    return { key, text: i > 0 ? w.slice(i + 2) : "", tone: key === "dmarc=fail" || key === "quarantined" ? "bad" : "warn" };
  });
}

/**
 * Whether the facts say nothing to act on: the trusted receiver passed
 * DMARC and nothing is downgraded, quarantined or warned about. Such mail
 * gets one line of facts instead of the box (§12.9), so that the box still
 * gets read when it does appear.
 */
export function isQuiet(m: Message, f: Facts | undefined): boolean {
  if (!f || m.parse_error || m.quarantined || m.downgraded || f.downgraded) return false;
  return !f.auth.withheld && f.auth.verified && f.auth.dmarc === "pass" && warnings(f).length === 0;
}
