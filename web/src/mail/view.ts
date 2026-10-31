import type { MessageListQuery, PrefixList } from "@/api/types";

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
  /** The draft open in the composer. */
  d?: string;
  /** A new message in the composer, before it is saved as a draft. */
  compose?: boolean;
  /** A reply to this message in the composer, before it is saved. */
  reply?: string;
  /** The outbox row open in the main column. */
  o?: string;
  /** The filter of the drafts or outbox list, in that list's terms. */
  st?: string;
}

/**
 * The sidebar's entries (§12.9). Conversations and notifications are the
 * two zones of §3 principle 4; drafts and outbox are what the viewer
 * writes and sends (§12.10); downgraded and unclaimed are the review
 * group, where the mail and prefixes the zones hide can still be found.
 */
export type Area = "conversations" | "notifications" | "all" | "drafts" | "outbox" | "downgraded" | "unclaimed";

export const areas: readonly Area[] = ["conversations", "notifications", "all", "drafts", "outbox", "downgraded", "unclaimed"];

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
    d: search.get("d") || undefined,
    compose: search.get("compose") === "1" || undefined,
    reply: search.get("reply") || undefined,
    o: search.get("o") || undefined,
    st: search.get("st") || undefined,
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
  // One composer at a time: a saved draft is its id from then on.
  if (v.d) p.set("d", v.d);
  else if (v.reply) p.set("reply", v.reply);
  else if (v.compose) p.set("compose", "1");
  if (v.o) p.set("o", v.o);
  if (v.st) p.set("st", v.st);
  const s = p.toString();
  return s ? `/?${s}` : "/";
}

/**
 * The query of GET /api/v1/messages for v, without a cursor;
 * undefined when the list column shows something else. Unknown mail goes
 * with conversations: a person's first mail is unknown, and missing it
 * costs more than a stray notification among conversations (§12.9).
 *
 * Opening one prefix means wanting all of its mail: a discovered prefix
 * has nothing but downgraded mail, and a hidden DMARC failure inside a
 * claimed one is exactly what to look at.
 */
export function listParams(v: View): MessageListQuery | undefined {
  if (v.prefix) return { prefix: v.prefix, downgraded: "include" };
  switch (v.area) {
    case "conversations":
      return { class: ["human", "unknown"], downgraded: "hide" };
    case "notifications":
      return { class: ["machine"], downgraded: "hide" };
    case "all":
      return { downgraded: v.dg ? "include" : "hide" };
    case "downgraded":
      return { downgraded: "only" };
    case "drafts":
    case "outbox":
    case "unclaimed":
      return undefined;
  }
}

/** Whether the composer is open; it takes the main column. */
export function composing(v: View): boolean {
  return !!(v.d || v.reply || v.compose);
}

/** Whether the main column shows something, which on a phone hides the list. */
export function mainOpen(v: View): boolean {
  return composing(v) || !!v.o || !!v.m;
}

/** What closing the composer changes: back to whatever it opened over. */
export const closeComposer: Partial<View> = { d: undefined, reply: undefined, compose: undefined };

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
