import { useVirtualizer } from "@tanstack/react-virtual";
import { useEffect, useRef } from "react";

import { useUnclaimedPrefixes } from "@/api/queries";
import { Skeleton } from "@/components/ui/skeleton";
import { count, listTime } from "@/lib/format";
import { m } from "@/paraglide/messages.js";
import { ProblemLine } from "@/send/marks";
import { problem } from "@/send/outgoing";

const rowHeight = 28;

/**
 * The unclaimed prefixes of §12.9, most recently delivered to first. A new
 * one near the top is either a leak or a service the operator forgot to
 * claim; the long tail is dictionary attacks. Picking one lists its mail.
 */
export function UnclaimedList({ onPick }: { onPick: (address: string) => void }) {
  const list = useUnclaimedPrefixes();
  const scroller = useRef<HTMLDivElement>(null);
  const rows = list.items.length + (list.hasMore ? 1 : 0);
  const v = useVirtualizer({ count: rows, getScrollElement: () => scroller.current, estimateSize: () => rowHeight, overscan: 20 });
  const items = v.getVirtualItems();
  const lastIndex = items.at(-1)?.index ?? -1;
  const { hasMore, isFetchingMore, loadMore } = list;
  useEffect(() => {
    if (hasMore && !isFetchingMore && lastIndex >= rows - 1) loadMore();
  }, [hasMore, isFetchingMore, lastIndex, rows, loadMore]);

  if (list.isLoading) {
    return (
      <div className="flex flex-col gap-3 p-3">
        {Array.from({ length: 8 }, (_, i) => (
          <Skeleton key={i} className="h-3.5 w-2/3" />
        ))}
      </div>
    );
  }
  if (list.error) {
    return (
      <p className="p-4">
        <ProblemLine p={problem(list.error)} />
      </p>
    );
  }
  if (list.items.length === 0) {
    return <p className="p-4 text-muted-foreground">{m.leak_no_unclaimed()}</p>;
  }

  return (
    <div ref={scroller} className="min-h-0 flex-1 overflow-y-auto py-1">
      <ul className="relative w-full" style={{ height: v.getTotalSize() }} aria-label={m.leak_unclaimed_label()}>
        {items.map((it) => {
          const p = list.items[it.index];
          return (
            <li key={it.key} className="absolute top-0 left-0 w-full" style={{ height: it.size, transform: `translateY(${it.start}px)` }}>
              {p ? (
                <button
                  type="button"
                  onClick={() => onPick(p.address)}
                  className="flex h-full w-full items-center gap-3 px-3 text-left outline-none hover:bg-muted focus-visible:bg-muted"
                >
                  <span className="min-w-0 flex-1 truncate font-mono text-small">{p.address}</span>
                  <span className="shrink-0 text-meta text-muted-foreground tabular-nums">{count(p.messages)}</span>
                  <span className="w-14 shrink-0 text-right text-meta text-muted-foreground tabular-nums">{p.last_seen ? listTime(p.last_seen) : ""}</span>
                </button>
              ) : (
                <div className="flex h-full items-center px-3 text-small text-muted-foreground">{m.leak_loading_more()}</div>
              )}
            </li>
          );
        })}
      </ul>
    </div>
  );
}
