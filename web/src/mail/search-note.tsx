import { ParaglideMessage } from "@inlang/paraglide-js-react";

import type { SearchList } from "@/api/queries";
import { markup } from "@/components/markup";
import { Mark } from "@/components/voice";
import { m as msg } from "@/paraglide/messages.js";

import { applyHint, echoDiffers, type QueryProblem } from "./search";

const problemWords: Record<QueryProblem["code"], () => string> = {
  invalid_param: () => msg.search_problem_invalid(),
  unsupported_filter: () => msg.search_problem_unsupported(),
  scope_required: () => msg.search_problem_scope(),
};

/**
 * How yuuka read the query, under the box (§16.5): in the system voice
 * (§12.5), since it is yuuka saying what it did with the words. Quiet
 * when the query was read as written; a line when the echo differs or an
 * item looks like a misspelt operator; the query with the offending item
 * marked when it cannot run.
 */
export function SearchNote({
  q,
  list,
  problem,
  onQuery,
}: {
  q: string;
  list: SearchList;
  problem?: QueryProblem;
  onQuery: (q: string) => void;
}) {
  if (problem) {
    const s = problem.span;
    return (
      <div role="alert" className="flex flex-col gap-0.5 text-small">
        <p className="flex items-baseline gap-1.5 text-destructive">
          <Mark kind="system" className="self-center" />
          {problemWords[problem.code]()}
        </p>
        <p className="pl-3.5 font-mono break-all whitespace-pre-wrap">
          {s ? (
            <>
              {q.slice(0, s.start)}
              <mark className="rounded-[2px] bg-destructive/15 text-destructive underline decoration-wavy underline-offset-2">
                {q.slice(s.start, s.end)}
              </mark>
              {q.slice(s.end)}
            </>
          ) : (
            q
          )}
        </p>
        {problem.detail && (
          <p className="pl-3.5 text-muted-foreground">
            {problem.detail}
            <span className="ml-1.5 font-mono text-meta">{problem.code}</span>
          </p>
        )}
      </div>
    );
  }
  const read = list.query !== undefined && echoDiffers(q, list.query) ? list.query : undefined;
  if (read === undefined && list.hints.length === 0) return null;
  return (
    <div className="flex flex-col gap-0.5 text-small" aria-live="polite">
      {read !== undefined && (
        <p className="flex min-w-0 items-baseline gap-1.5">
          <Mark kind="system" className="self-center" />
          <span className="shrink-0 text-system-foreground">{msg.search_read_as()}</span>
          <code className="min-w-0 font-mono break-all">{read}</code>
        </p>
      )}
      {list.hints.map((h) => {
        const fixed = applyHint(q, h);
        return (
          <p key={h.term} className="flex min-w-0 flex-wrap items-baseline gap-x-1.5">
            <Mark kind="system" className="self-center" />
            <span className="min-w-0 text-system-foreground">
              <ParaglideMessage
                message={msg.search_hint}
                inputs={{ term: h.term, suggest: h.suggest }}
                markup={markup}
              />
            </span>
            {fixed !== undefined && (
              <button
                type="button"
                className="underline underline-offset-2 hover:text-foreground"
                onClick={() => onQuery(fixed)}
              >
                {msg.search_hint_apply({ suggest: h.suggest })}
              </button>
            )}
          </p>
        );
      })}
    </div>
  );
}
