import { usePrefixes } from "@/api/queries";
import type { Prefix } from "@/api/types.gen";
import { Skeleton } from "@/components/ui/skeleton";
import { count } from "@/lib/format";
import { cn } from "@/lib/utils";

import type { View } from "./view";

/**
 * The aggregation view of §3.3: every receiving prefix with how much mail
 * it got. Claimed prefixes first; discovered ones (first seen in mail,
 * everything to them downgraded) below, since a new one there is either
 * a leak or a service the operator forgot to claim.
 */
export function PrefixPane({ view, onPick }: { view: View; onPick: (next: Partial<View>) => void }) {
  const q = usePrefixes();
  const ps = q.data?.prefixes ?? [];
  const claimed = ps.filter((p) => p.claimed);
  const discovered = ps.filter((p) => !p.claimed);
  const all = !view.prefix;

  return (
    <nav aria-label="Prefixes" className="flex min-h-0 flex-1 flex-col overflow-y-auto py-2 text-[13px]">
      <Item active={all && view.dg !== "only"} label="All mail" n={q.data && q.data.messages - q.data.downgraded} onClick={() => onPick({ prefix: undefined, dg: "hide" })} />
      <Item
        active={all && view.dg === "only"}
        label="Downgraded"
        muted
        n={q.data?.downgraded}
        onClick={() => onPick({ prefix: undefined, dg: "only" })}
      />
      {q.isLoading && (
        <div className="flex flex-col gap-2 px-3 pt-4">
          <Skeleton className="h-3.5 w-2/3" />
          <Skeleton className="h-3.5 w-1/2" />
        </div>
      )}
      <Group title="Claimed" ps={claimed} view={view} onPick={onPick} />
      <Group title="Discovered" ps={discovered} view={view} onPick={onPick} />
    </nav>
  );
}

function Group({ title, ps, view, onPick }: { title: string; ps: Prefix[]; view: View; onPick: (next: Partial<View>) => void }) {
  if (ps.length === 0) return null;
  // One zone is the common case; then the zone is noise on every row.
  const zones = new Set(ps.map((p) => p.zone));
  return (
    <>
      <h2 className="mt-4 mb-1 px-3 text-[11px] font-medium tracking-wide text-muted-foreground uppercase">{title}</h2>
      {ps.map((p) => (
        <Item
          key={p.address}
          active={view.prefix === p.address}
          label={zones.size > 1 ? p.address : p.prefix}
          hint={p.label}
          mono
          muted={!p.claimed}
          n={p.claimed ? p.messages - p.downgraded : p.messages}
          onClick={() => onPick({ prefix: p.address, dg: "hide" })}
        />
      ))}
    </>
  );
}

function Item(props: { active: boolean; label: string; hint?: string; n?: number; mono?: boolean; muted?: boolean; onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={props.onClick}
      aria-current={props.active || undefined}
      className={cn(
        "mx-1.5 flex h-7 items-center gap-2 rounded-md px-2 text-left hover:bg-muted",
        props.active && "bg-accent text-accent-foreground hover:bg-accent",
      )}
    >
      <span className={cn("min-w-0 truncate", props.mono && "font-mono text-[12px]", props.muted && !props.active && "text-muted-foreground")}>
        {props.label}
      </span>
      {props.hint && <span className="min-w-0 truncate text-[11px] text-muted-foreground">{props.hint}</span>}
      {props.n !== undefined && <span className="ml-auto shrink-0 text-[11px] text-muted-foreground tabular-nums">{count(props.n)}</span>}
    </button>
  );
}
