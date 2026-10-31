import type { Delivery, Message } from "@/api/types.gen";

import { fullTime } from "@/lib/format";

// What the system knows about a message, as opposed to what the message
// says about itself. These render in the system voice: they come from the
// trusted receiver, the prefix registry and the parser, never from
// headers the sender wrote.

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

function authParts(d: Delivery): Part[] {
  const a = d.auth;
  if (!a) return [{ text: "not recorded", tone: "plain" }];
  if (!a.trusted) return [{ text: `unverified${a.reason ? ` (${a.reason})` : ""}`, tone: "warn" }];
  const parts: Part[] = [];
  const first = (method: string, prop?: string) =>
    a.results?.find((r) => r.method === method && (!prop || (r.props && prop in r.props)));
  const shown = [
    ["dmarc", first("dmarc")],
    ["spf", first("spf", "smtp.mailfrom") ?? first("spf")],
    ["dkim", first("dkim")],
  ] as const;
  for (const [name, r] of shown) {
    if (r) parts.push({ text: `${name}=${r.result}`, tone: resultTone(r.result) });
  }
  if (parts.length === 0) parts.push({ text: "no results", tone: "warn" });
  parts.push({ text: `by ${a.authserv_id}`, tone: "plain" });
  return parts;
}

export function systemFacts(m: Message): Fact[] {
  const facts: Fact[] = [];
  if (m.quarantined) facts.push({ label: "Quarantined", parts: [{ text: m.parse_error || "parse problem", tone: "bad" }] });
  if (m.downgraded) {
    const claimed = m.deliveries?.some((d) => d.prefix_claimed);
    const reason = m.deliveries === undefined ? "hidden from default views" : claimed ? "DMARC failed" : "only sent to unclaimed prefixes";
    facts.push({ label: "Downgraded", parts: [{ text: reason, tone: "warn" }] });
  }
  for (const d of m.deliveries ?? []) {
    const label = m.deliveries && m.deliveries.length > 1 ? `Auth · ${d.prefix}` : "Auth";
    facts.push({ label, parts: authParts(d) });
  }
  facts.push({ label: "Received", parts: [{ text: fullTime(m.received_at), tone: "plain" }] });
  facts.push({ label: "Class", parts: [{ text: m.class, tone: "plain" }] });
  return facts;
}
