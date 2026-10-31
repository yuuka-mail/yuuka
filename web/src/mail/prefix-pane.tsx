import { useState } from "react";

import { useDrafts, useOutbox, usePrefixes, useUnclaimedPrefixes } from "@/api/queries";
import type { Prefix } from "@/api/types";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { count } from "@/lib/format";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";
import type { Abilities } from "@/send/outgoing";

import { LeakDot, leakSummary, leakTone, useFlaggedCount, type LeakTone } from "./leaks";
import { areaTitle } from "./areas";
import type { View } from "./view";

// Past this many claimed prefixes the list gets a filter box: about what
// fits beside the zones and the review group on a laptop screen.
const filterFrom = 20;

/**
 * The sidebar of §12.9: where attention goes, not a directory of every
 * address. The zones on top; the claimed prefixes in the middle, in a
 * stable alphabetical order; the review group pinned at the bottom, so it
 * stays in reach however many prefixes there are. Unclaimed prefixes are
 * a count here and a paged list of their own: a catch-all collects
 * thousands of them.
 */
export function PrefixPane({ view, can, onPick }: { view: View; can: Abilities; onPick: (next: Partial<View>) => void }) {
  const q = usePrefixes();
  const unclaimed = useUnclaimedPrefixes();
  // Counts only for what asks for action (§12.9): open drafts, and outbox
  // rows only a person can settle.
  const drafts = useDrafts(["draft", "held"]);
  const unknown = useOutbox(["unknown"], can.outbox);
  const flaggedCount = useFlaggedCount();
  const browsing = !view.prefix && !view.q;
  const at = (area: View["area"]) => browsing && view.area === area;

  return (
    <nav aria-label={m.nav_mailboxes()} className="flex min-h-0 flex-1 flex-col text-ui">
      <div className="flex shrink-0 flex-col py-2">
        <Item active={at("conversations")} label={areaTitle("conversations")} onClick={() => onPick({ area: "conversations" })} />
        <Item active={at("notifications")} label={areaTitle("notifications")} onClick={() => onPick({ area: "notifications" })} />
        <Item active={at("all")} label={areaTitle("all")} onClick={() => onPick({ area: "all" })} />
        <Item
          active={at("drafts")}
          label={areaTitle("drafts")}
          n={drafts.items.length || undefined}
          more={drafts.hasMore}
          onClick={() => onPick({ area: "drafts" })}
        />
        {can.outbox && (
          <Item
            active={at("outbox")}
            label={areaTitle("outbox")}
            n={unknown.items.length || undefined}
            more={unknown.hasMore}
            warn
            title={unknown.items.length ? m.nav_outbox_unknown() : undefined}
            onClick={() => onPick({ area: "outbox" })}
          />
        )}
      </div>
      <Claimed ps={q.data?.prefixes} active={view.prefix} onPick={onPick} />
      <div className="flex shrink-0 flex-col border-t py-2">
        <Heading>{m.nav_review()}</Heading>
        <Item active={at("downgraded")} label={areaTitle("downgraded")} n={q.data?.downgraded} onClick={() => onPick({ area: "downgraded" })} />
        <Item active={at("unclaimed")} label={areaTitle("unclaimed")} n={unclaimed.total} onClick={() => onPick({ area: "unclaimed" })} />
        <Item
          active={at("leaks")}
          label={areaTitle("leaks")}
          n={flaggedCount || undefined}
          warn
          title={flaggedCount ? m.nav_leaks_title() : undefined}
          onClick={() => onPick({ area: "leaks" })}
        />
      </div>
    </nav>
  );
}

function Claimed({ ps, active, onPick }: { ps?: Prefix[]; active?: string; onPick: (next: Partial<View>) => void }) {
  const [filter, setFilter] = useState("");
  if (!ps) {
    return (
      <div className="flex flex-1 flex-col gap-2 px-3 pt-4">
        <Skeleton className="h-3.5 w-2/3" />
        <Skeleton className="h-3.5 w-1/2" />
      </div>
    );
  }
  // One zone is the common case; then the zone is noise on every row.
  const zones = new Set(ps.map((p) => p.zone));
  const f = filter.trim().toLowerCase();
  const shown = ps
    .filter((p) => !f || p.address.includes(f) || p.label.toLowerCase().includes(f))
    .toSorted((a, b) => a.prefix.localeCompare(b.prefix) || a.zone.localeCompare(b.zone));
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <Heading>{m.shell_prefixes()}</Heading>
      {ps.length > filterFrom && (
        <div className="px-1.5 pb-1">
          <Input value={filter} onChange={(e) => setFilter(e.target.value)} placeholder={m.nav_filter()} aria-label={m.nav_filter_prefixes()} className="h-7" />
        </div>
      )}
      <div className="flex min-h-0 flex-1 flex-col overflow-y-auto pb-2">
        {shown.map((p) => (
          <Item
            key={p.address}
            active={active === p.address}
            label={zones.size > 1 ? p.address : p.prefix}
            hint={p.label}
            mono
            mark={leakTone(p)}
            title={leakSummary(p) || undefined}
            n={p.messages - p.downgraded}
            onClick={() => onPick({ prefix: p.address })}
          />
        ))}
        {shown.length === 0 && <p className="px-3 text-small text-muted-foreground">{ps.length ? m.nav_no_match() : m.nav_no_claimed()}</p>}
      </div>
    </div>
  );
}

function Heading({ children }: { children: string }) {
  return <h2 className="mt-2 mb-1 px-3 text-meta font-medium tracking-wide text-muted-foreground uppercase">{children}</h2>;
}

function Item(props: {
  active: boolean;
  label: string;
  hint?: string;
  n?: number;
  more?: boolean;
  warn?: boolean;
  title?: string;
  mono?: boolean;
  mark?: LeakTone;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      title={props.title}
      onClick={props.onClick}
      aria-current={props.active || undefined}
      className={cn(
        "mx-1.5 flex h-7 shrink-0 items-center gap-2 rounded-md px-2 text-left hover:bg-muted",
        props.active && "bg-accent text-accent-foreground hover:bg-accent",
      )}
    >
      <span className={cn("min-w-0 truncate", props.mono && "font-mono text-small")}>{props.label}</span>
      {props.hint && <span className="min-w-0 truncate text-meta text-muted-foreground">{props.hint}</span>}
      {props.mark && (
        <>
          <LeakDot tone={props.mark} className="ml-auto" />
          <span className="sr-only">{props.title}</span>
        </>
      )}
      {props.n !== undefined && (
        <span className={cn("shrink-0 text-meta tabular-nums", !props.mark && "ml-auto", props.warn ? "font-medium text-warning" : "text-muted-foreground")}>
          {count(props.n)}
          {props.more && "+"}
        </span>
      )}
    </button>
  );
}
