import type { Class, PrefixList } from "@/api/types.gen";

/**
 * What the mail screen shows, kept entirely in the query string so that a
 * link (from Telegram, say) opens exactly this view, and back/forward
 * work. The first navigation from such a link carries no session cookie
 * (SameSite=Strict); the shell loads anyway and fetches state afterwards.
 */
export interface View {
  /** prefix@zone, or undefined for all mail. */
  prefix?: string;
  cls?: Class;
  /** The §4.3 downgrade filter. */
  dg: Downgraded;
  /** Search text; when set, the list shows search hits. */
  q?: string;
  /** The open message. */
  m?: string;
}

export type Downgraded = "hide" | "include" | "only";

const classes: readonly Class[] = ["machine", "human", "unknown"];
const downgrades: readonly Downgraded[] = ["hide", "include", "only"];

export function parseView(search: URLSearchParams): View {
  const cls = search.get("class") as Class | null;
  const dg = search.get("dg") as Downgraded | null;
  return {
    prefix: search.get("prefix") || undefined,
    cls: cls && classes.includes(cls) ? cls : undefined,
    dg: dg && downgrades.includes(dg) ? dg : "hide",
    q: search.get("q")?.trim() || undefined,
    m: search.get("m") || undefined,
  };
}

export function viewHref(v: View): string {
  const p = new URLSearchParams();
  if (v.prefix) p.set("prefix", v.prefix);
  if (v.cls) p.set("class", v.cls);
  if (v.dg !== "hide") p.set("dg", v.dg);
  if (v.q) p.set("q", v.q);
  if (v.m) p.set("m", v.m);
  const s = p.toString();
  return s ? `/?${s}` : "/";
}

/**
 * The downgrade filter the API gets. Opening one prefix means wanting all
 * of its mail: a discovered prefix has nothing but downgraded mail, and a
 * hidden DMARC failure inside a claimed one is exactly what to look at.
 */
export function effectiveDowngrade(v: View): Downgraded {
  return v.prefix && v.dg === "hide" ? "include" : v.dg;
}

/** The query string of GET /api/v1/messages for v, without a cursor. */
export function listParams(v: View): URLSearchParams {
  const p = new URLSearchParams();
  if (v.prefix) p.set("prefix", v.prefix);
  if (v.cls) p.set("class", v.cls);
  p.set("downgraded", effectiveDowngrade(v));
  return p;
}

/**
 * How many messages v lists in all, when the prefix aggregation already
 * says so; undefined for class-filtered views, which it does not count.
 */
export function knownTotal(v: View, ps: PrefixList | undefined): number | undefined {
  if (!ps || v.q || v.cls) return undefined;
  if (v.prefix) return ps.prefixes.find((p) => p.address === v.prefix)?.messages;
  if (v.dg === "only") return ps.downgraded;
  return v.dg === "include" ? ps.messages : ps.messages - ps.downgraded;
}
