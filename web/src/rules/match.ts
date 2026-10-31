import type { RuleAction } from "@/api/types";

// A rule's match tree (§8.5) as the WebUI reads and edits it. The
// contract types it as a plain object because it is recursive; this is
// its shape, the store format v1: exactly one of all, any, not or a leaf.
// The field set is closed and mirrors internal/rules/fields.go; the
// server validates every tree again and says where it is wrong.

export type Leaf = { field: string; op: string; value: unknown };
export type RuleMatch = { all: RuleMatch[] } | { any: RuleMatch[] } | { not: RuleMatch } | Leaf;

export type FieldType = "text" | "bool" | "enum" | "set";

export interface FieldSpec {
  type: FieldType;
  /** enum and set: the values it takes. */
  values?: readonly string[];
}

export const fields = {
  "from.addr": { type: "text" },
  "from.domain": { type: "text" },
  "from.org": { type: "text" },
  "from.verified": { type: "bool" },
  "to.addr": { type: "text" },
  "to.prefix": { type: "text" },
  "to.zone": { type: "text" },
  "prefix.claimed": { type: "bool" },
  verdict: { type: "set", values: ["leak", "spoof"] },
  subject: { type: "text" },
  body: { type: "text" },
  "list.id": { type: "text" },
  class: { type: "enum", values: ["machine", "human", "unknown"] },
  extraction: { type: "set", values: ["otp", "link", "expiry", "amount"] },
  attachment: { type: "bool" },
} as const satisfies Record<string, FieldSpec>;

export type Field = keyof typeof fields;
export const fieldNames = Object.keys(fields) as Field[];

export const opsOf: Record<FieldType, readonly string[]> = {
  text: ["eq", "in", "contains", "regex"],
  bool: ["eq"],
  enum: ["eq", "in"],
  set: ["has"],
};

export function isField(f: string): f is Field {
  return f in fields;
}

export function isLeaf(m: RuleMatch): m is Leaf {
  return "field" in m;
}

/** The match tree of an API object, or undefined when it is not one. */
export function asMatch(x: unknown): RuleMatch | undefined {
  if (!x || typeof x !== "object" || Array.isArray(x)) return undefined;
  const o = x as Record<string, unknown>;
  const keys = Object.keys(o);
  if (keys.length === 1 && (keys[0] === "all" || keys[0] === "any")) {
    const list = o[keys[0]];
    if (!Array.isArray(list)) return undefined;
    const kids = list.map(asMatch);
    if (kids.some((k) => !k)) return undefined;
    return keys[0] === "all" ? { all: kids as RuleMatch[] } : { any: kids as RuleMatch[] };
  }
  if (keys.length === 1 && keys[0] === "not") {
    const inner = asMatch(o.not);
    return inner && { not: inner };
  }
  if (keys.length === 3 && typeof o.field === "string" && typeof o.op === "string" && "value" in o) {
    return { field: o.field, op: o.op, value: o.value };
  }
  return undefined;
}

// The editor's form of a tree: groups and leaves, each of which may be
// negated. A tree it cannot hold (a not of a not, a field this version
// does not know) is edited as JSON instead.

export type EditNode =
  | { id: number; kind: "all" | "any"; not: boolean; children: EditNode[] }
  | { id: number; kind: "leaf"; not: boolean; field: Field; op: string; value: unknown };

let nextID = 1;
export const newID = () => nextID++;

export function toEdit(m: RuleMatch): EditNode | undefined {
  if ("not" in m) {
    const inner = toEdit(m.not);
    if (!inner || inner.not) return undefined;
    return { ...inner, not: true };
  }
  if ("all" in m || "any" in m) {
    const kind = "all" in m ? "all" : "any";
    const kids = ("all" in m ? m.all : m.any).map(toEdit);
    if (kids.some((k) => !k)) return undefined;
    return { id: newID(), kind, not: false, children: kids as EditNode[] };
  }
  if (!isField(m.field) || !opsOf[fields[m.field].type].includes(m.op)) return undefined;
  return { id: newID(), kind: "leaf", not: false, field: m.field, op: m.op, value: m.value };
}

export function fromEdit(n: EditNode): RuleMatch {
  const m: RuleMatch =
    n.kind === "leaf" ? { field: n.field, op: n.op, value: n.value } : n.kind === "all" ? { all: n.children.map(fromEdit) } : { any: n.children.map(fromEdit) };
  return n.not ? { not: m } : m;
}

/** A new condition on f, with an operator and a value of the right kind. */
export function newLeaf(f: Field): EditNode {
  const spec: FieldSpec = fields[f];
  const op = opsOf[spec.type][0] ?? "eq";
  return { id: newID(), kind: "leaf", not: false, field: f, op, value: defaultValue(spec, op) };
}

export function defaultValue(spec: FieldSpec, op: string): unknown {
  if (spec.type === "bool") return true;
  if (op === "in") return spec.values ? [spec.values[0]] : [];
  if (spec.values) return spec.values[0];
  return "";
}

/** Values of an in condition as typed: separated by commas or new lines. */
export function splitValues(s: string): string[] {
  return s
    .split(/[\n,]/)
    .map((x) => x.trim())
    .filter(Boolean);
}

/** The match of a draft's ticked conditions: all of them, or none. */
export function allOf(conds: Leaf[]): RuleMatch | undefined {
  return conds.length ? { all: conds } : undefined;
}

/** Whether actions can be saved: at least one, and every label named. */
export function actionsComplete(as: RuleAction[]): boolean {
  return as.length > 0 && as.every((a) => (a.kind === "label" ? !!a.label?.trim() : a.kind === "classify" ? !!a.class : a.kind === "notify" ? !!a.level : true));
}

/**
 * The actions a rule can take for each annotation a person or an agent
 * made by hand. Undoing kinds (unlabel, unarchive, unclassify) have no
 * rule action: rules only add (§8.5).
 */
export function ruleActionsOf(as: { kind: string; label?: string; class?: string }[]): RuleAction[] {
  const out: RuleAction[] = [];
  for (const a of as) {
    if (a.kind === "label" && a.label) out.push({ kind: "label", label: a.label });
    else if (a.kind === "archive") out.push({ kind: "archive" });
    else if (a.kind === "classify" && (a.class === "machine" || a.class === "human")) out.push({ kind: "classify", class: a.class });
  }
  return out;
}
