import { useOutbox } from "@/api/queries";
import type { OutboxEntry } from "@/api/types";
import { Skeleton } from "@/components/ui/skeleton";
import { listTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";

import { AuthorMark, OutgoingMark, toneText } from "./marks";
import { outboxFilters, problem, recipientState, type OutboxFilter } from "./outgoing";
import { HeldLeft } from "./review";

export function parseOutboxFilter(st: string | undefined): OutboxFilter {
  return st && st in outboxFilters ? (st as OutboxFilter) : "all";
}

/**
 * Outgoing mail, newest first, each row with every recipient's state.
 * The subject is the sent copy's, which the server reads for the row.
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
  if (list.items.length === 0) return <p className="p-4 text-muted-foreground">{m.send_outbox_empty()}</p>;
  return (
    <div className="min-h-0 flex-1 overflow-y-auto">
      <ul aria-label={m.send_outbox_list_label()}>
        {list.items.map((e) => (
          <li key={e.id} className="h-14">
            <Row e={e} selected={e.id === selected} onSelect={onSelect} />
          </li>
        ))}
      </ul>
      {list.hasMore && (
        <button type="button" className="w-full py-2 text-small text-muted-foreground hover:text-foreground" disabled={list.isFetchingMore} onClick={list.loadMore}>
          {list.isFetchingMore ? m.send_loading() : m.send_load_more()}
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
        {/* The subject leads. From follows it whole or not at all: when
            it does not fit, it wraps onto a line that is clipped, since
            a From cut to a few letters says nothing (the detail has it). */}
        <span className="flex h-[1lh] min-w-0 flex-wrap items-baseline overflow-hidden">
          <span className="min-w-0 truncate font-medium">{e.subject || <span className="font-normal text-muted-foreground">{m.send_no_subject()}</span>}</span>
          <span className="shrink-0 font-mono text-small whitespace-pre text-muted-foreground"> · {e.from}</span>
        </span>
        {e.submitted_by.kind !== "human" && <AuthorMark author={e.submitted_by} />}
        <span className="ml-auto flex shrink-0 items-center gap-2 pl-2 text-meta text-muted-foreground tabular-nums">
          <OutgoingMark state={e.state} />
          {/* Held mail goes by itself: how long is left matters more than when it was submitted. */}
          {e.state === "held" && e.hold_until ? <HeldLeft until={e.hold_until} /> : listTime(e.created_at)}
        </span>
      </span>
      <Recipients rs={e.recipients} />
    </button>
  );
}

/**
 * Every recipient with its own state: a partial send is visible at a
 * glance. When they do not fit, the others give way first, down to an
 * ellipsis, so the first address keeps its state however long it is.
 */
function Recipients({ rs }: { rs: OutboxEntry["recipients"] }) {
  const [first, ...rest] = rs;
  if (!first) return null;
  return (
    <span className="flex min-w-0 items-baseline text-small">
      <Recipient r={first} className={cn("flex shrink-0 items-baseline gap-1", rest.length > 0 ? "max-w-[calc(100%-3ch)]" : "max-w-full")} />
      {rest.length > 0 && (
        <span className="min-w-0 truncate">
          {rest.map((r) => (
            <span key={`${r.kind}:${r.addr}`}>
              <span className="text-muted-foreground">, </span>
              <Recipient r={r} />
            </span>
          ))}
        </span>
      )}
    </span>
  );
}

function Recipient({ r, className }: { r: OutboxEntry["recipients"][number]; className?: string }) {
  const s = recipientState(r.state);
  return (
    <span className={className}>
      <span className="min-w-0 truncate font-mono">{r.addr}</span> <span className={cn("shrink-0", toneText[s.tone])}>{s.label}</span>
    </span>
  );
}
