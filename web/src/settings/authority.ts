import type { AppliedGrant, ManagedGrant, RecipientScope, Tier, TightenBody } from "@/api/types";
import { m } from "@/paraglide/messages.js";
import { duration, principal, reviewText, scopeText, tierLabel } from "@/send/outgoing";

// What the settings page offers on a grant (§13.10): only what is tighter
// than what applies now, column by column, in the order the server
// compares them (store/tighten.go). The server checks it all again and
// refuses anything wider with the host command; offering only tighter
// values here means a person rarely sees that refusal. Pure, so tested.

const tierOrder: Tier[] = ["none", "draft", "review", "auto"];

/** The tiers below t, highest first. */
export function lowerTiers(t: Tier): Tier[] {
  return tierOrder.slice(0, Math.max(0, tierOrder.indexOf(t))).toReversed();
}

/**
 * The scopes narrower than s. Scope is a partial order: thread and
 * allowlist are both narrower than any and incomparable with each other,
 * so either way between them is a widening, done on the host.
 */
export function narrowerScopes(s: RecipientScope): RecipientScope[] {
  return s === "any" ? ["thread", "allowlist"] : [];
}

/** One change to offer, with the body that makes it. */
export type Tightening =
  | { kind: "suspend" }
  | { kind: "tier"; tier: Tier }
  | { kind: "scope"; scope: RecipientScope }
  | { kind: "allowlist" }
  | { kind: "limit"; field: "max_per_hour" | "max_per_day" | "max_new_recipients_per_day" }
  | { kind: "hold" };

export const limitFields = ["max_per_hour", "max_per_day", "max_new_recipients_per_day"] as const;

/** Every change that only tightens grant a, in the order the page lists them. */
export function tightenings(a: AppliedGrant): Tightening[] {
  const out: Tightening[] = [];
  if (!a.suspended) out.push({ kind: "suspend" });
  for (const tier of lowerTiers(a.send_policy)) out.push({ kind: "tier", tier });
  for (const scope of narrowerScopes(a.recipient_scope)) out.push({ kind: "scope", scope });
  if (a.allowlist.length > 0) out.push({ kind: "allowlist" });
  for (const field of limitFields) {
    // A limit of 0 lets nothing through; there is nothing lower.
    if (a.limits[field] !== 0) out.push({ kind: "limit", field });
  }
  // The window only matters on the review tier: it is how long mail the
  // checks passed waits for a recall.
  if (a.send_policy === "review") out.push({ kind: "hold" });
  return out;
}

/** Whether n is a tighter limit than the current one; unlimited is wider than any number. */
export function lowerLimit(current: number | undefined, n: number): boolean {
  return Number.isInteger(n) && n >= 0 && (current === undefined || n < current);
}

/** Whether keep is a strict subset of the allowlist, compared as the server does, ignoring case. */
export function subsetOf(allowlist: string[], keep: string[]): boolean {
  const all = new Set(allowlist.map((x) => x.toLowerCase()));
  return keep.length < all.size && keep.every((x) => all.has(x.toLowerCase()));
}

/** The request body of one tightening; values come from the form. */
export function tightenBody(t: Tightening, v: { limit?: number; holdMinutes?: number; keep?: string[] }): TightenBody {
  switch (t.kind) {
    case "suspend":
      return { suspended: true };
    case "tier":
      return { send_policy: t.tier };
    case "scope":
      return { recipient_scope: t.scope };
    case "allowlist":
      return { allowlist: v.keep ?? [] };
    case "limit":
      return { [t.field]: v.limit };
    case "hold":
      return { hold_seconds: (v.holdMinutes ?? 0) * 60 };
  }
}

/**
 * What a grant lets through: its tier, and the columns that bound it.
 * dormant: the tier sends nothing by itself (none, draft), so the other
 * columns take effect only if the tier is raised on the host; they are
 * still compared when tightening (§13.10).
 */
export function grantParts(a: AppliedGrant): { tier: string; rest: string[]; dormant: boolean } {
  const l = a.limits;
  return {
    tier: a.send_policy === "review" ? reviewText(a.hold_seconds) : tierLabel(a.send_policy),
    rest: [
      m.send_to_scope({ scope: scopeText(a.recipient_scope, a.allowlist) }),
      l.max_per_hour === undefined ? m.grant_unlimited_hour() : m.send_limit_hour({ n: l.max_per_hour }),
      l.max_per_day === undefined ? m.grant_unlimited_day() : m.send_limit_day({ n: l.max_per_day }),
      l.max_new_recipients_per_day === undefined ? m.grant_unlimited_new() : m.grant_limit_new({ n: l.max_new_recipients_per_day }),
    ],
    dormant: a.send_policy === "none" || a.send_policy === "draft",
  };
}

/** A grant in one line: "review — held 10 min after the checks · to any recipient · 10/hour"; the tier alone when nothing goes out by itself. */
export function grantSummary(a: AppliedGrant): string {
  const p = grantParts(a);
  return p.dormant ? p.tier : [p.tier, ...p.rest].join(" · ");
}

/** What tightening t costs, said beside the second press. */
export function tighteningCost(g: ManagedGrant, t: Tightening, v: { limit?: number; holdMinutes?: number }): string {
  const who = principal(g.principal);
  const as = g.identity.address;
  switch (t.kind) {
    case "suspend":
      return m.grant_cost_suspend({ who, as });
    case "tier":
      return t.tier === "none" ? m.grant_cost_none({ who, as }) : t.tier === "draft" ? m.grant_cost_draft({ who, as }) : m.grant_cost_review({ who, as });
    case "scope":
      return t.scope === "thread" ? m.grant_cost_thread({ who, as }) : m.grant_cost_allowlist({ who, as });
    case "allowlist":
      return m.grant_cost_allowlist_remove({ who, as });
    case "limit":
      return m.grant_cost_limit({ who, as });
    case "hold":
      return m.grant_cost_hold({ window: duration((v.holdMinutes ?? 0) * 60) });
  }
}

/** The host command that removes grant g (§13.10), from the one that sets it. */
export function removeCommand(g: ManagedGrant): string {
  return g.host.replace(/^yuuka grant set /, "yuuka grant rm ");
}
