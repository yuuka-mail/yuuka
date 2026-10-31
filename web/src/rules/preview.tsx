import { useEffect, useState } from "react";

import { useRulePreview } from "@/api/queries";
import type { RuleAction, RuleWarning } from "@/api/types";
import { Signature, system } from "@/components/voice";
import { count, listTime, sender } from "@/lib/format";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";
import { ProblemLine } from "@/send/marks";
import { problem } from "@/send/outgoing";

import type { RuleMatch } from "./match";

/** value, once it has stopped changing for ms. */
export function useDebounced<T>(value: T, ms = 400): T {
  const [v, setV] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return v;
}

/**
 * What a rule would do to the mail here now, before it is saved
 * (§12.12): the same evaluator and the same count as `yuuka rule apply
 * --dry-run`, said by yuuka, with a few of the messages it matches.
 */
export function RulePreviewBox({
  match,
  actions,
  onOpen,
}: {
  match?: RuleMatch;
  actions: RuleAction[];
  onOpen?: (id: string) => void;
}) {
  const key = match && actions.length ? JSON.stringify({ match, actions }) : "";
  const settled = useDebounced(key);
  const q = useRulePreview(
    settled
      ? {
          ...(JSON.parse(settled) as { match: Record<string, unknown>; actions: RuleAction[] }),
          samples: 5,
        }
      : undefined,
  );
  if (!match) return null;
  // What it would change depends on what it does; until then, say so
  // rather than count.
  if (!key)
    return <p className="text-small text-muted-foreground">{m.rule_preview_needs_action()}</p>;
  const p = q.data;
  return (
    <section
      aria-label={m.rule_preview_label()}
      className={cn(
        "flex flex-col gap-1 rounded-md border border-system-border bg-system-surface px-2 py-1.5 text-small",
        q.isFetching && "opacity-80",
      )}
    >
      <p className="flex flex-wrap items-center gap-x-2">
        <Signature speaker={system} />
        {p ? (
          <span>
            {m.rule_preview_counts({ matched: count(p.matched), messages: count(p.messages) })}
            {p.matched > 0 && (
              <span className="text-muted-foreground">
                {" · "}
                {m.rule_preview_changed({ changed: count(p.changed) })}
                {p.overruled > 0 &&
                  ` · ${m.rule_preview_overruled({ overruled: count(p.overruled) })}`}
              </span>
            )}
            {p.failed > 0 && (
              <span className="text-warning">
                {" "}
                · {m.rule_preview_failed({ failed: count(p.failed) })}
              </span>
            )}
          </span>
        ) : q.error ? (
          <ProblemLine p={problem(q.error)} />
        ) : (
          <span aria-hidden className="inline-block h-3.5 w-48 animate-pulse rounded-md bg-muted" />
        )}
      </p>
      {p && <Warnings warnings={p.warnings} />}
      {p && p.samples.length > 0 && (
        <ul
          aria-label={m.rule_preview_samples()}
          className="flex flex-col border-t border-system-border pt-1"
        >
          {p.samples.map((s) => (
            <li key={s.id}>
              <button
                type="button"
                disabled={!onOpen}
                onClick={() => onOpen?.(s.id)}
                className="flex w-full min-w-0 items-baseline gap-2 rounded-sm text-left enabled:hover:bg-muted"
              >
                <span className="w-32 shrink-0 truncate">{sender(s.from_name, s.from_addr)}</span>
                <span lang="" className="min-w-0 flex-1 truncate text-muted-foreground">
                  {s.subject || m.no_subject()}
                </span>
                <span className="shrink-0 text-meta text-muted-foreground tabular-nums">
                  {listTime(s.received_at)}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/**
 * What a rule allows but probably does not mean, by code; the server's
 * sentence is a note (§12.11).
 */
export function Warnings({ warnings }: { warnings: RuleWarning[] }) {
  if (!warnings.length) return null;
  return (
    <ul className="flex flex-col gap-0.5">
      {warnings.map((w) => (
        <li key={w.code} className="text-warning">
          <span className="font-medium">
            {w.code === "unverified_from" ? m.rule_warn_unverified_from() : m.rule_warn_other()}
          </span>
          <span className="text-muted-foreground"> · {w.message}</span>
        </li>
      ))}
    </ul>
  );
}
