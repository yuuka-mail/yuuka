import { useOutbox } from "@/api/queries";
import type { OutboxEntry } from "@/api/types";
import { Skeleton } from "@/components/ui/skeleton";
import { listTime } from "@/lib/format";
import { cn } from "@/lib/utils";

import { OutgoingMark, toneText } from "./marks";
import { outboxFilters, problem, recipientState, type OutboxFilter } from "./outgoing";

export function parseOutboxFilter(st: string | undefined): OutboxFilter {
  return st && st in outboxFilters ? (st as OutboxFilter) : "all";
}

/**
 * Outgoing mail, newest first, each row with every recipient's state.
 * The outbox has no subject of its own (the sent copy has it), so a row
 * names who it went to and from where; the detail opens the sent copy.
 */
export function OutboxList({ filter, selected, onSelect }: { filter: OutboxFilter; selected?: string; onSelect: (id: string) => void }) {
  const states = outboxFilters[filter].states;
  const list = useOutbox(states ? [...states] : undefined);
  if (list.isLoading) {
    return (
      <div className="flex flex-col gap-4 p-3">
        {Array.from({ length: 4 }, (_, i) => (
          <div key={i} className="flex flex-col gap-1.5">
            <Skeleton className="h-3.5 w-1/3" />
            <Skeleton className="h-3.5 w-4/5" />
          </div>
        ))}
      </div>
    );
  }
  if (list.error) return <p className="p-4 text-destructive">{problem(list.error).text}</p>;
  if (list.items.length === 0) return <p className="p-4 text-muted-foreground">Nothing in the outbox here.</p>;
  return (
    <div className="min-h-0 flex-1 overflow-y-auto">
      <ul aria-label="Outbox">
        {list.items.map((e) => (
          <li key={e.id} className="h-14">
            <Row e={e} selected={e.id === selected} onSelect={onSelect} />
          </li>
        ))}
      </ul>
      {list.hasMore && (
        <button type="button" className="w-full py-2 text-small text-muted-foreground hover:text-foreground" disabled={list.isFetchingMore} onClick={list.loadMore}>
          {list.isFetchingMore ? "Loading…" : "Load more"}
        </button>
      )}
    </div>
  );
}

function Row({ e, selected, onSelect }: { e: OutboxEntry; selected: boolean; onSelect: (id: string) => void }) {
  return (
    <button
      type="button"
      aria-current={selected || undefined}
      onClick={() => onSelect(e.id)}
      className={cn(
        "flex h-full w-full flex-col justify-center gap-0.5 border-b px-3 text-left outline-none hover:bg-muted focus-visible:bg-muted",
        selected && "bg-accent text-accent-foreground shadow-[inset_2px_0_0_var(--color-foreground)] hover:bg-accent",
      )}
    >
      <span className="flex min-w-0 items-center gap-1.5 text-ui">
        <span className="min-w-0 truncate font-mono text-small">{e.from}</span>
        {e.submitted_by.kind !== "human" && <span className="shrink-0 text-meta text-muted-foreground">by {e.submitted_by.name}</span>}
        <span className="ml-auto flex shrink-0 items-center gap-2 pl-2 text-meta text-muted-foreground tabular-nums">
          <OutgoingMark state={e.state} />
          {listTime(e.created_at)}
        </span>
      </span>
      {/* Every recipient with its own state: a partial send is visible at a glance. */}
      <span className="truncate text-small">
        {e.recipients.map((r, i) => {
          const s = recipientState(r.state);
          return (
            <span key={`${r.kind}:${r.addr}`}>
              {i > 0 && <span className="text-muted-foreground">, </span>}
              <span className="font-mono">{r.addr}</span> <span className={toneText[s.tone]}>{s.label}</span>
            </span>
          );
        })}
      </span>
    </button>
  );
}
