import type { TriageAnswer, TriageReport } from "@/api/types";
import { m } from "@/paraglide/messages.js";

// The words for a scorer's estimates (§15): what each question and option
// is called, how a probability reads, which leanings a list row marks.
// Pure, so the wording is tested and the components only lay it out.
// Everything here is the observation voice (§12.5): a model's estimate,
// which acts on nothing.

// Functions, not strings: the words are the current locale's (§12.11).
// A question or option this table does not know (a newer question set,
// a backend's extra) shows as its key, like any identifier.
const questions: Record<string, () => string> = {
  kind: m.triage_q_kind,
  phishing: m.triage_q_phishing,
  needs_reply: m.triage_q_needs_reply,
};
const options: Record<string, () => string> = {
  yes: m.triage_o_yes,
  no: m.triage_o_no,
  notification: m.triage_o_notification,
  conversation: m.triage_o_conversation,
  marketing: m.triage_o_marketing,
  spam: m.triage_o_spam,
};

export const questionLabel = (q: string) => questions[q]?.() ?? q;
export const optionLabel = (o: string) => options[o]?.() ?? o;

/** The set's order (§15.3), then any other question by name. */
const order = ["kind", "phishing", "needs_reply"];
const rank = (q: string) => (order.includes(q) ? order.indexOf(q) : order.length);
export function byQuestion<T extends { question: string }>(xs: readonly T[]): T[] {
  return xs.toSorted(
    (a, b) => rank(a.question) - rank(b.question) || a.question.localeCompare(b.question),
  );
}

/**
 * A probability as a whole percentage. The ends never round to 0% or
 * 100%: a model is never certain, and "100%" would read as a verdict.
 */
export function pct(p: number): string {
  if (p > 0.99) return ">99%";
  if (p < 0.01) return "<1%";
  return `${Math.round(p * 100)}%`;
}

/** The options of an answer, most probable first. */
export function ranked(a: TriageAnswer): { option: string; p: number }[] {
  return Object.entries(a.p ?? {})
    .map(([option, p]) => ({ option, p }))
    .toSorted((x, y) => y.p - x.p || x.option.localeCompare(y.option));
}

/**
 * The leanings a list row marks: the ones worth a glance while scanning
 * (§15.4), not every answer. A row says what the model leans to and
 * nothing more; the probability is in the reading pane.
 */
const notable: readonly (readonly [string, string])[] = [
  ["phishing", "yes"],
  ["kind", "spam"],
];

export function notableLeanings(tops: Record<string, string> | undefined): Lean[] {
  if (!tops) return [];
  return notable
    .filter(([q, o]) => tops[q] === o)
    .map(([question, option]) => ({ question, option }));
}

/** One question leaning to one option: the triage= filter. */
export interface Lean {
  question: string;
  option: string;
}

const leanPattern = /^([a-z][a-z0-9_]*):([a-z][a-z0-9_]*)$/;

/** The triage= value of the URL, or undefined for anything else. */
export function parseLean(s: string | null | undefined): Lean | undefined {
  const x = s ? leanPattern.exec(s) : null;
  return x?.[1] && x[2] ? { question: x[1], option: x[2] } : undefined;
}

export const leanParam = (l: Lean) => `${l.question}:${l.option}`;

/** What a leaning reads as in a sentence: "phishing: yes". */
export function leanLabel(l: Lean): string {
  return m.triage_lean({ question: questionLabel(l.question), option: optionLabel(l.option) });
}

/** What a row's mark says: the yes of a yes/no question is the question itself. */
export function markLabel(l: Lean): string {
  return m.triage_mark({
    what: l.option === "yes" ? questionLabel(l.question) : optionLabel(l.option),
  });
}

/** The list filter's default: the shadow list §15.5 asks for first. */
export const defaultLean: Lean = { question: "phishing", option: "yes" };

/**
 * The leanings to filter by: every option the report counted, in the
 * set's order, most frequent first within a question; the current one
 * stays offered even when nothing leans to it any more.
 */
export function leanChoices(
  report: TriageReport | undefined,
  current: Lean | undefined,
): (Lean & { messages?: number })[] {
  const out: (Lean & { messages?: number })[] = byQuestion(report?.answers ?? []).flatMap((a) =>
    a.options.map((o) => ({ question: a.question, option: o.option, messages: o.messages })),
  );
  if (current && !out.some((l) => leanParam(l) === leanParam(current))) out.unshift(current);
  return out;
}
