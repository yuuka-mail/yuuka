import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { api, data } from "@/api/client";
import { invalidateAnnotated, useAnnotations, useRules } from "@/api/queries";
import type { AnnotationEntry, Message, UndoResult } from "@/api/types";
import { Skeleton } from "@/components/ui/skeleton";
import { fullTime, listTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";
import { ActionWords } from "@/rules/words";
import { ProblemLine } from "@/send/marks";
import { problem, type Abilities } from "@/send/outgoing";

import { UndoResultText } from "./annotate";

/**
 * Why this message is where it is (§8.5, §8.8): every change to its
 * labels, class and archived state, oldest first, with who made it at
 * what rank. A rule speaks as yuuka (it is the person's standing
 * decision run by the system), an agent in its own voice (§12.5). A
 * change can be undone action by action; changed again since, it is
 * reported as superseded and left as it is.
 */
export function History({ message, can, onOpenRule }: { message: Message; can: Abilities; onOpenRule?: (id: string) => void }) {
  const q = useAnnotations(message.id);
  const rules = useRules(true, can.rules);
  const ruleName = (id: string) => rules.data?.rules.find((r) => r.id === id)?.name || id;
  const h = q.data?.history ?? [];
  // The last row of each action is where its undo sits.
  const lastOfAction = new Map(h.map((e, i) => [e.action_id, i]));
  return (
    <section aria-label={m.history_label()} className="rounded-md border px-3 py-2 text-small">
      <p className="mb-1 text-muted-foreground">{summary(message, h)}</p>
      {q.error ? (
        <ProblemLine p={problem(q.error)} />
      ) : !q.data ? (
        <Skeleton className="h-10 w-full" />
      ) : h.length === 0 ? (
        <p className="text-muted-foreground">{m.history_empty()}</p>
      ) : (
        <ol className="flex flex-col gap-0.5">
          {h.map((e, i) => (
            <Entry key={e.seq} e={e} ruleName={ruleName} onOpenRule={can.rules ? onOpenRule : undefined} undo={lastOfAction.get(e.action_id) === i && !e.undoes && !e.undone_by && can.annotate} />
          ))}
        </ol>
      )}
    </section>
  );
}

/** Where the message stands and why, in one sentence. */
function summary(msg: Message, h: AnnotationEntry[]): string {
  const live = h.filter((e) => !e.undone_by && !e.undoes);
  const cls = live.findLast((e) => e.change.kind === "classify");
  const zone = msg.class === "machine" ? m.area_notifications() : m.area_conversations();
  if (msg.archived) return m.history_archived();
  return cls ? m.history_moved({ zone, who: who(cls) }) : m.history_classifier({ zone });
}

function who(e: AnnotationEntry): string {
  return e.by.kind === "rule" ? m.history_a_rule() : e.by.name;
}

function Entry({ e, ruleName, onOpenRule, undo }: { e: AnnotationEntry; ruleName: (id: string) => string; onOpenRule?: (id: string) => void; undo: boolean }) {
  const qc = useQueryClient();
  const [state, setState] = useState<UndoResult | "busy">();
  const [err, setErr] = useState<unknown>();
  const gone = !!e.undone_by;
  return (
    <li className={cn("flex min-w-0 flex-wrap items-baseline gap-x-2", gone && "text-muted-foreground line-through decoration-muted-foreground/50")}>
      <span className="w-12 shrink-0 text-meta text-muted-foreground tabular-nums" title={fullTime(e.at)}>
        {listTime(e.at)}
      </span>
      <By e={e} ruleName={ruleName} onOpenRule={onOpenRule} />
      <span className="min-w-0">
        {e.undoes ? <span className="text-muted-foreground">{m.history_undo_of()} </span> : null}
        <ActionWords a={e.change} />
      </span>
      <span className="font-mono text-meta text-muted-foreground no-underline">{e.tool}</span>
      {undo && state === undefined && (
        <button
          type="button"
          className="ml-auto underline underline-offset-2"
          onClick={async () => {
            setState("busy");
            try {
              setState(await data(api.POST("/api/v1/actions/{id}/undo", { params: { path: { id: e.action_id } } })));
              invalidateAnnotated(qc);
            } catch (x) {
              setErr(x);
              setState(undefined);
            }
          }}
        >
          {m.undo()}
        </button>
      )}
      {state && state !== "busy" && <UndoResultText r={state} />}
      {err !== undefined && <ProblemLine p={problem(err)} />}
    </li>
  );
}

/** Who made a change: a rule as yuuka's square, an agent as its dot, a person as is. */
function By({ e, ruleName, onOpenRule }: { e: AnnotationEntry; ruleName: (id: string) => string; onOpenRule?: (id: string) => void }) {
  if (e.by.kind === "rule") {
    const name = ruleName(e.by.name);
    return (
      <span className="inline-flex min-w-0 items-center gap-1 text-system-foreground">
        <span aria-hidden className="size-1.5 shrink-0 rounded-[1px] bg-system" />
        {onOpenRule ? (
          <button type="button" className="truncate underline underline-offset-2" onClick={() => onOpenRule(e.by.name)}>
            {m.history_rule({ name })}
          </button>
        ) : (
          <span className="truncate">{m.history_rule({ name })}</span>
        )}
      </span>
    );
  }
  if (e.rank === "agent") {
    return (
      <span className="inline-flex items-center gap-1 font-medium text-agent-foreground">
        <span aria-hidden className="size-1.5 shrink-0 rounded-full bg-agent" />
        {e.by.name}
      </span>
    );
  }
  return <span className="font-medium">{e.by.kind === "system" ? m.history_host() : e.by.name}</span>;
}
