import { ApiError } from "@/api/client";

import { leanOf, type View } from "./view";

// The search box's side of the query text (§16): writing a view's scope
// into the box, completing a value under the caret, finding where the
// server says a query went wrong. The grammar itself is the server's
// (internal/search); this reads only as much of it as the box needs.

/**
 * A value as one item of the text form: quoted only when it would not
 * read back as one word, the server's quoteValue, so that a prefilled
 * scope echoes back as written.
 */
export function quoteValue(v: string): string {
  if (v !== "" && !/[\s"()]/u.test(v)) return v;
  return `"${v.replace(/[\\"]/g, (c) => `\\${c}`)}"`;
}

/**
 * The scope of v as query text: what searching from it starts with, in the
 * box where the person sees and can delete it (§16.5). Undefined for the
 * zones and all mail, whose search spans both zones (§12.9), and for the
 * lists that are not mail.
 */
export function scopeOf(v: View): string | undefined {
  if (v.prefix) return `deliveredto:${quoteValue(v.prefix)}`;
  if (v.label) return `label:${quoteValue(v.label)}`;
  switch (v.area) {
    case "archived":
      return "is:archived";
    case "downgraded":
      return "is:downgraded";
    case "leanings": {
      const l = leanOf(v);
      return l ? `triage:${l}` : undefined;
    }
    default:
      return undefined;
  }
}

/** The operators whose values the box offers to complete. */
export type Completable = "label" | "deliveredto" | "class" | "is" | "has" | "triage";
const completable = new Set<string>(["label", "deliveredto", "class", "is", "has", "triage"]);

/** The value of an operator under the caret, and the text it spans. */
export interface ValueAt {
  op: Completable;
  /** What is typed of the value so far, unquoted. */
  typed: string;
  /** The value's span in the text, quotes included: what a pick replaces. */
  start: number;
  end: number;
}

/**
 * The operator value the caret is in, if it is one the box completes.
 * Items are split as the server splits them: at white space outside
 * quotes; a leading - or ( belongs to the item but not to its name.
 */
export function valueAt(text: string, caret: number): ValueAt | undefined {
  let start = 0;
  let quoted = false;
  for (let i = 0; i <= text.length; i++) {
    const c = text[i];
    if (quoted && c === "\\") {
      i++;
      continue;
    }
    if (c === '"') quoted = !quoted;
    if (i < text.length && (quoted || !/\s/u.test(c ?? ""))) continue;
    // An item ends at i.
    if (start <= caret && caret <= i && start < i) return itemValue(text, start, i, caret);
    start = i + 1;
  }
  return undefined;
}

function itemValue(text: string, start: number, end: number, caret: number) {
  const item = text.slice(start, end);
  const x = /^[-(]*([A-Za-z]+):/.exec(item);
  const name = x?.[1]?.toLowerCase();
  if (!x || !name || !completable.has(name)) return undefined;
  const vStart = start + x[0].length;
  if (caret < vStart) return undefined;
  let raw = text.slice(vStart, end);
  let vEnd = end;
  if (raw.startsWith('"')) {
    const closed = raw.length > 1 && raw.endsWith('"') && !raw.endsWith('\\"');
    raw = raw.slice(1, closed ? -1 : undefined).replace(/\\(["\\])/g, "$1");
  } else {
    // A ) closing a group is not part of the value.
    const bare = raw.replace(/\)+$/, "");
    vEnd -= raw.length - bare.length;
    raw = bare;
    if (caret > vEnd) return undefined;
  }
  return { op: name as Completable, typed: raw, start: vStart, end: vEnd };
}

/** One value the box offers: what goes into the query, and how it reads. */
export interface Choice {
  value: string;
  label?: string;
  /** How many messages carry it, when known (labels, prefixes). */
  count?: number;
}

/**
 * The choices for a value as typed: those containing it, ignoring case,
 * in the order given. None when the value is already one of them, so
 * that a complete value does not keep a list open.
 */
export function matching(choices: readonly Choice[], typed: string, max = 8): Choice[] {
  const t = typed.toLowerCase();
  if (choices.some((c) => c.value === typed)) return [];
  return choices
    .filter((c) => c.value.toLowerCase().includes(t) || !!c.label?.toLowerCase().includes(t))
    .slice(0, max);
}

/**
 * text with the value at v replaced by choice, and where the caret goes:
 * after it and a space, so the next item can be typed straight away.
 */
export function pick(text: string, v: ValueAt, choice: string): { text: string; caret: number } {
  const before = text.slice(0, v.start) + quoteValue(choice);
  const after = text.slice(v.end);
  const sep = after === "" || !/^[\s)]/u.test(after) ? " " : "";
  return { text: before + sep + after, caret: before.length + sep.length };
}

/** text with item appended as a new item, as the help's examples add one. */
export function append(text: string, item: string): string {
  const t = text.trimEnd();
  return (t ? `${t} ${item}` : item) + " ";
}

/**
 * Whether the server read q differently from how it was written, beyond
 * white space: then the echo is worth a line (§16.5). A relative time
 * always is, as the echo says the time it stands for.
 */
export function echoDiffers(q: string, normalized: string): boolean {
  return squash(q) !== squash(normalized);
}

const squash = (s: string) => s.trim().replace(/\s+/gu, " ");

/**
 * q with a hint taken: the item written as term gets the operator the
 * server suggests. A suggested name (from:) replaces the name only; a
 * whole condition (has:attachment) replaces the item. Undefined when the
 * term is not an item of q as written.
 */
export function applyHint(q: string, h: { term: string; suggest: string }): string | undefined {
  const at = findItem(q, h.term);
  if (at < 0) return undefined;
  const colon = h.term.indexOf(":");
  const replaced =
    h.suggest.endsWith(":") && colon > 0 ? h.suggest + h.term.slice(colon + 1) : h.suggest;
  return q.slice(0, at) + replaced + q.slice(at + h.term.length);
}

function findItem(q: string, term: string): number {
  for (let i = q.indexOf(term); i >= 0; i = q.indexOf(term, i + 1)) {
    const prev = q[i - 1];
    const next = q[i + term.length];
    if ((prev === undefined || /[\s(-]/u.test(prev)) && (next === undefined || /[\s)]/u.test(next)))
      return i;
  }
  return -1;
}

/** A query the server could not run as written, and where. */
export interface QueryProblem {
  code: "invalid_param" | "unsupported_filter" | "scope_required";
  /** The server's sentence, without the q: and position it starts with. */
  detail: string;
  /** Where the offending item is in q, as UTF-16 offsets; absent for the query as a whole. */
  span?: { start: number; end: number };
}

/**
 * The problem with q in err, when err is about q. The server gives the
 * position only in its detail, "q: at character 5: …", counted in
 * characters from 1 (§16.2); the item runs from there to the next space.
 */
export function queryProblem(err: unknown, q: string): QueryProblem | undefined {
  if (!(err instanceof ApiError)) return undefined;
  const code = err.code;
  if (code !== "invalid_param" && code !== "unsupported_filter" && code !== "scope_required")
    return undefined;
  const x = /^q: (?:at character (\d+): )?([\s\S]*)$/.exec(err.message);
  if (!x) return undefined;
  const detail = x[2] ?? "";
  const at = x[1] ? Number(x[1]) : 0;
  const chars = Array.from(q);
  if (at < 1 || at > chars.length) return { code, detail };
  const start = chars.slice(0, at - 1).join("").length;
  const rest = q.slice(start);
  const len = /^\S*/u.exec(rest)?.[0].length || 1;
  return { code, detail, span: { start, end: start + len } };
}
