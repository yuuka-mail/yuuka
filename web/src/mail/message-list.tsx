import { useVirtualizer } from "@tanstack/react-virtual";
import { PaperclipIcon, TriangleAlertIcon } from "lucide-react";
import { useEffect, useRef } from "react";

import type { MailList } from "@/api/queries";
import type { MessageSummary } from "@/api/types";
import { Skeleton } from "@/components/ui/skeleton";
import { listTime, localPart, sender } from "@/lib/format";
import { cn } from "@/lib/utils";

const rowHeight = 56;

/**
 * The virtualised message list (§12.3): catch-all mail runs to tens of
 * thousands of rows, so only the visible ones exist in the DOM. The next
 * page loads when the last rows come into view.
 */
export function MessageList({
  list,
  selected,
  markNotifications,
  onSelect,
}: {
  list: MailList;
  selected?: string;
  /** Search spans both zones (§12.9): tag the rows from notifications. */
  markNotifications?: boolean;
  onSelect: (id: string) => void;
}) {
  const scroller = useRef<HTMLDivElement>(null);
  const rows = list.items.length + (list.hasMore ? 1 : 0);
  const v = useVirtualizer({
    count: rows,
    getScrollElement: () => scroller.current,
    estimateSize: () => rowHeight,
    overscan: 12,
  });
  const items = v.getVirtualItems();
  const lastIndex = items.at(-1)?.index ?? -1;
  const { hasMore, isFetchingMore, loadMore } = list;
  useEffect(() => {
    if (hasMore && !isFetchingMore && lastIndex >= rows - 1) loadMore();
  }, [hasMore, isFetchingMore, lastIndex, rows, loadMore]);

  // Keep the selection in view when it moves by keyboard.
  const selectedIndex = selected ? list.items.findIndex((m) => m.id === selected) : -1;
  useEffect(() => {
    if (selectedIndex >= 0) v.scrollToIndex(selectedIndex, { align: "auto" });
  }, [selectedIndex, v]);

  if (list.isLoading) {
    return (
      <div className="flex flex-col gap-4 p-3">
        {Array.from({ length: 8 }, (_, i) => (
          <div key={i} className="flex flex-col gap-1.5">
            <Skeleton className="h-3.5 w-1/3" />
            <Skeleton className="h-3.5 w-4/5" />
          </div>
        ))}
      </div>
    );
  }
  if (list.error) {
    return <p className="p-4 text-destructive">{list.error instanceof Error ? list.error.message : "Could not load mail."}</p>;
  }
  if (list.items.length === 0) {
    return <p className="p-4 text-muted-foreground">No mail here.</p>;
  }

  return (
    <div ref={scroller} className="min-h-0 flex-1 overflow-y-auto" role="listbox" aria-label="Messages">
      <div className="relative w-full" style={{ height: v.getTotalSize() }}>
        {items.map((it) => {
          const m = list.items[it.index];
          return (
            <div key={it.key} className="absolute top-0 left-0 w-full" style={{ height: it.size, transform: `translateY(${it.start}px)` }}>
              {m ? (
                <Row m={m} selected={m.id === selected} tag={markNotifications && m.class === "machine"} onSelect={onSelect} />
              ) : (
                <div className="flex h-full items-center px-3 text-small text-muted-foreground">Loading more…</div>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}

function Row({ m, selected, tag, onSelect }: { m: MessageSummary; selected: boolean; tag?: boolean; onSelect: (id: string) => void }) {
  return (
    <button
      type="button"
      role="option"
      aria-selected={selected}
      onClick={() => onSelect(m.id)}
      className={cn(
        "flex h-full w-full flex-col justify-center gap-0.5 border-b px-3 text-left outline-none hover:bg-muted focus-visible:bg-muted",
        selected && "bg-accent text-accent-foreground shadow-[inset_2px_0_0_var(--color-foreground)] hover:bg-accent",
        m.downgraded && !selected && "text-muted-foreground",
      )}
    >
      <span className="flex min-w-0 items-center gap-1.5 text-ui">
        <span className={cn("min-w-0 truncate", m.class === "human" ? "font-semibold" : "font-medium")}>{sender(m.from_name, m.from_addr)}</span>
        {m.recipients.length > 0 && (
          <span className="shrink-0 truncate font-mono text-meta text-muted-foreground">→ {m.recipients.map(localPart).join(", ")}</span>
        )}
        {tag && <span className="shrink-0 rounded-sm bg-muted px-1 text-meta text-muted-foreground">notification</span>}
        <span className="ml-auto flex shrink-0 items-center gap-1 pl-2 text-meta text-muted-foreground tabular-nums">
          {m.quarantined && <TriangleAlertIcon aria-label="quarantined" className="size-3 text-warning" />}
          {m.has_attach && <PaperclipIcon aria-label="attachment" className="size-3" />}
          {listTime(m.received_at)}
        </span>
      </span>
      <span className="truncate text-ui">
        {m.subject || <span className="text-muted-foreground">(no subject)</span>}
        {m.snippet && <span className="text-muted-foreground"> — {m.snippet}</span>}
      </span>
    </button>
  );
}
