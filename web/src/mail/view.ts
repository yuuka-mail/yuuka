import type { PrefixList } from "@/api/types.gen";

/**
 * What the mail screen shows, kept entirely in the query string so that a
 * link (from Telegram, say) opens exactly this view, and back/forward
 * work. The first navigation from such a link carries no session cookie
 * (SameSite=Strict); the shell loads anyway and fetches state afterwards.
 */
export interface View {
  /** What the list shows when no prefix is picked and nothing searched. */
  area: Area;
  /** prefix@zone: that prefix's mail, whatever its class. */
  prefix?: string;
  /** All mail and search: include downgraded mail too. */
  dg: boolean;
  /** Search text; when set, the list shows search hits. */
  q?: string;
  /** The open message. */
  m?: string;
  /**
   * An anchor in the open message's structured view, such as
   * part/1/para/3: the fragment of yuuka://message/<m>#part/1/para/3.
   */
  a?: string;
}

/**
 * The sidebar's entries (§12.9). Conversations and notifications are the
 * two zones of §3 principle 4; downgraded and unclaimed are the review
 * group, where the mail and prefixes the zones hide can still be found.
 */
export type Area = "conversations" | "notifications" | "all" | "downgraded" | "unclaimed";

export const areas: readonly Area[] = ["conversations", "notifications", "all", "downgraded", "unclaimed"];

// The anchor grammar of §8.1, without yuuka://message/<id>#.
const anchorPattern = /^part\/[1-9][0-9]*(\.[1-9][0-9]*)*(\/para\/[1-9][0-9]*)?$/;

export function parseView(search: URLSearchParams): View {
  const area = search.get("area") as Area | null;
  const m = search.get("m") || undefined;
  const a = search.get("a") ?? "";
  return {
    area: area && areas.includes(area) ? area : "conversations",
    prefix: search.get("prefix") || undefined,
    dg: search.get("dg") === "include",
    q: search.get("q")?.trim() || undefined,
    m,
    a: m && anchorPattern.test(a) ? a : undefined,
  };
}

export function viewHref(v: View): string {
  const p = new URLSearchParams();
  if (v.area !== "conversations") p.set("area", v.area);
  if (v.prefix) p.set("prefix", v.prefix);
  if (v.dg) p.set("dg", "include");
  if (v.q) p.set("q", v.q);
  if (v.m) p.set("m", v.m);
  if (v.m && v.a) p.set("a", v.a);
  const s = p.toString();
  return s ? `/?${s}` : "/";
}

/**
 * The query string of GET /api/v1/messages for v, without a cursor;
 * undefined when the list column shows something else. Unknown mail goes
 * with conversations: a person's first mail is unknown, and missing it
 * costs more than a stray notification among conversations (§12.9).
 *
 * Opening one prefix means wanting all of its mail: a discovered prefix
 * has nothing but downgraded mail, and a hidden DMARC failure inside a
 * claimed one is exactly what to look at.
 */
export function listParams(v: View): URLSearchParams | undefined {
  if (v.prefix) return new URLSearchParams({ prefix: v.prefix, downgraded: "include" });
  switch (v.area) {
    case "conversations":
      return new URLSearchParams({ class: "human,unknown", downgraded: "hide" });
    case "notifications":
      return new URLSearchParams({ class: "machine", downgraded: "hide" });
    case "all":
      return new URLSearchParams({ downgraded: v.dg ? "include" : "hide" });
    case "downgraded":
      return new URLSearchParams({ downgraded: "only" });
    case "unclaimed":
      return undefined;
  }
}

/**
 * How many messages v lists in all, when the prefix aggregation already
 * says so; undefined for the zones, which it does not count by class.
 */
export function knownTotal(v: View, ps: PrefixList | undefined): number | undefined {
  if (!ps || v.q) return undefined;
  if (v.prefix) return ps.prefixes.find((p) => p.address === v.prefix)?.messages;
  if (v.area === "downgraded") return ps.downgraded;
  if (v.area === "all") return v.dg ? ps.messages : ps.messages - ps.downgraded;
  return undefined;
}
