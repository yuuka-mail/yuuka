import type { ReactNode } from "react";

import type { Annotation, RuleAction } from "@/api/types";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";

import { isField, isLeaf, type Field, type Leaf, type RuleMatch } from "./match";

// A rule in plain words (§12.12): what a person reads before saving one.
// Values are data, from mail or from an agent's proposal, so they render
// as text, in the mono face that sets them apart from our words.

export function fieldName(f: string): string {
  if (!isField(f)) return f;
  const names: Record<Field, () => string> = {
    "from.addr": m.rule_field_from_addr,
    "from.domain": m.rule_field_from_domain,
    "from.org": m.rule_field_from_org,
    "from.verified": m.rule_field_from_verified,
    "to.addr": m.rule_field_to_addr,
    "to.prefix": m.rule_field_to_prefix,
    "to.zone": m.rule_field_to_zone,
    "prefix.claimed": m.rule_field_prefix_claimed,
    verdict: m.rule_field_verdict,
    subject: m.rule_field_subject,
    body: m.rule_field_body,
    "list.id": m.rule_field_list_id,
    class: m.rule_field_class,
    extraction: m.rule_field_extraction,
    attachment: m.rule_field_attachment,
  };
  return names[f]();
}

export function opName(op: string): string {
  switch (op) {
    case "eq":
      return m.rule_op_eq();
    case "in":
      return m.rule_op_in();
    case "contains":
      return m.rule_op_contains();
    case "regex":
      return m.rule_op_regex();
    case "has":
      return m.rule_op_has();
    default:
      return op;
  }
}

/** A value of a condition: booleans in words, everything else as the data it is. */
export function Value({ v }: { v: unknown }) {
  if (typeof v === "boolean")
    return <span className="font-medium">{v ? m.rule_yes() : m.rule_no()}</span>;
  const list = Array.isArray(v) ? v : [v];
  return (
    <>
      {list.map((x, i) => (
        <span key={i}>
          {i > 0 && ", "}
          <span className="rounded-sm bg-muted px-1 font-mono text-small break-words">
            {String(x)}
          </span>
        </span>
      ))}
    </>
  );
}

export function LeafWords({ leaf, className }: { leaf: Leaf; className?: string }) {
  return (
    <span className={cn("inline", className)}>
      <span className="font-medium">{fieldName(leaf.field)}</span>{" "}
      <span className="text-muted-foreground">{opName(leaf.op)}</span> <Value v={leaf.value} />
    </span>
  );
}

/**
 * The whole tree: a condition inline, a group as a list under "all of"
 * or "any of", a negation with "not" in front.
 */
export function MatchWords({ match, className }: { match: RuleMatch; className?: string }) {
  return <div className={cn("text-small", className)}>{node(match)}</div>;
}

function node(x: RuleMatch, negated = false): ReactNode {
  if ("not" in x) return node(x.not, !negated);
  const not = negated && <span className="font-semibold text-warning">{m.rule_not()} </span>;
  if (isLeaf(x)) {
    return (
      <span>
        {not}
        <LeafWords leaf={x} />
      </span>
    );
  }
  const all = "all" in x;
  const kids = all ? x.all : x.any;
  return (
    <div>
      {not}
      <span className="text-muted-foreground">{all ? m.rule_all_of() : m.rule_any_of()}</span>
      <ul className="ml-1 flex flex-col gap-0.5 border-l pl-2.5">
        {kids.map((k, i) => (
          <li key={i}>{node(k)}</li>
        ))}
      </ul>
    </div>
  );
}

export function notifyName(level: string | undefined): string {
  switch (level) {
    case "off":
      return m.rule_notify_off();
    case "silent":
      return m.rule_notify_silent();
    case "normal":
      return m.rule_notify_normal();
    default:
      return level ?? "";
  }
}

/** One action, as words with its label as data. */
export function ActionWords({ a }: { a: RuleAction | Annotation }) {
  switch (a.kind) {
    case "label":
    case "unlabel":
      return (
        <span>
          {a.kind === "label" ? m.rule_action_label() : m.rule_action_unlabel()}{" "}
          <LabelChip label={a.label ?? ""} />
        </span>
      );
    case "archive":
      return <span>{m.rule_action_archive()}</span>;
    case "unarchive":
      return <span>{m.rule_action_unarchive()}</span>;
    case "classify":
      return (
        <span>
          {a.class === "machine"
            ? m.rule_action_to_notifications()
            : a.class === "human"
              ? m.rule_action_to_conversations()
              : m.rule_action_classify({ class: a.class ?? "" })}
        </span>
      );
    case "unclassify":
      return <span>{m.rule_action_unclassify()}</span>;
    case "notify":
      return <span>{notifyName("level" in a ? a.level : undefined)}</span>;
  }
}

export function ActionsWords({
  actions,
  className,
}: {
  actions: (RuleAction | Annotation)[];
  className?: string;
}) {
  return (
    <span className={cn("inline-flex flex-wrap items-center gap-x-1.5 gap-y-0.5", className)}>
      {actions.map((a, i) => (
        <span key={i} className="inline-flex items-center gap-1.5">
          {i > 0 && <span className="text-muted-foreground">·</span>}
          <ActionWords a={a} />
        </span>
      ))}
    </span>
  );
}

/**
 * A label as it is stored: an exact, case-sensitive string someone typed
 * or a rule set, shown as data.
 */
export function LabelChip({
  label,
  className,
  children,
}: {
  label: string;
  className?: string;
  children?: ReactNode;
}) {
  return (
    <span
      className={cn(
        "inline-flex h-5 max-w-56 min-w-0 items-center gap-0.5 rounded-sm border bg-muted/60 px-1.5 text-small",
        className,
      )}
    >
      <span className="truncate">{label}</span>
      {children}
    </span>
  );
}
