import { ParaglideMessage } from "@inlang/paraglide-js-react";
import { useState } from "react";

import { markup } from "@/components/markup";
import { usePrefixDetail, usePrefixes } from "@/api/queries";
import type { CountedAs, Offense, Prefix } from "@/api/types";
import { Skeleton } from "@/components/ui/skeleton";
import { Signature, system } from "@/components/voice";
import { count, listTime, sender } from "@/lib/format";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";

import { ClaimButton, CountedAsNotes, ExpectEditor, UnclaimButton } from "./prefix-actions";

// The leak check of §3.3 as yuuka states it: which claimed prefixes got
// mail from senders they were not given to (a leak) or mail claiming one
// of those senders without DMARC proving it (a spoof). Both are facts the
// store computes, so they speak in the system voice.

export type LeakTone = "bad" | "warn";

/** A spoof outranks a leak: it is mail pretending to be someone. */
export function leakTone(p: Prefix): LeakTone | undefined {
  return p.spoofs > 0 ? "bad" : p.leaks > 0 ? "warn" : undefined;
}

/** "2 leaked (1 unproven) · 1 spoofed"; empty when nothing offended. */
export function leakSummary(p: Prefix): string {
  const out: string[] = [];
  if (p.leaks) out.push(leakedText(p));
  if (p.spoofs) out.push(m.leak_spoofed({ n: count(p.spoofs) }));
  return out.join(" · ");
}

function leakedText(p: Prefix): string {
  return p.unverified
    ? m.leak_leaked_unproven({ n: count(p.leaks), unproven: count(p.unverified) })
    : m.leak_leaked({ n: count(p.leaks) });
}

/** leakSummary in colour: a leak is a caution, a spoof an error. */
function LeakCounts({ p }: { p: Prefix }) {
  return (
    <span className="font-medium">
      {p.leaks > 0 && <span className="text-warning">{leakedText(p)}</span>}
      {p.leaks > 0 && p.spoofs > 0 && <span className="text-muted-foreground"> · </span>}
      {p.spoofs > 0 && (
        <span className="text-destructive">{m.leak_spoofed({ n: count(p.spoofs) })}</span>
      )}
    </span>
  );
}

/** The mark of a prefix whose mail leaked or was spoofed. */
export function LeakDot({ tone, className }: { tone: LeakTone; className?: string }) {
  return (
    <span
      aria-hidden
      className={cn(
        "size-1.5 shrink-0 rounded-full",
        tone === "bad" ? "bg-destructive" : "bg-warning",
        className,
      )}
    />
  );
}

const flagged = (ps: Prefix[] | undefined) =>
  (ps ?? [])
    .filter((p) => p.leaks + p.spoofs > 0)
    .toSorted((a, b) => (b.last_offense ?? "").localeCompare(a.last_offense ?? ""));

/** How many claimed prefixes the review group's entry counts. */
export function useFlaggedCount(): number | undefined {
  const q = usePrefixes();
  return q.data && flagged(q.data.prefixes).length;
}

/**
 * The review list of claimed prefixes whose mail leaked or was spoofed,
 * latest offence first. Picking one lists its mail, with its facts on top.
 */
export function LeakList({ onPick }: { onPick: (address: string) => void }) {
  const q = usePrefixes();
  if (!q.data) {
    return (
      <div className="flex flex-col gap-3 p-3">
        <Skeleton className="h-3.5 w-2/3" />
        <Skeleton className="h-3.5 w-1/2" />
      </div>
    );
  }
  const ps = flagged(q.data.prefixes);
  if (ps.length === 0) return <p className="p-4 text-muted-foreground">{m.leak_none()}</p>;
  return (
    <ul className="min-h-0 flex-1 overflow-y-auto" aria-label={m.leak_list_label()}>
      {ps.map((p) => {
        const strangers = p.observed.filter((o) => !o.expected).map((o) => o.domain);
        return (
          <li key={p.address}>
            <button
              type="button"
              onClick={() => onPick(p.address)}
              className="flex h-14 w-full flex-col justify-center gap-0.5 border-b px-3 text-left outline-none hover:bg-muted focus-visible:bg-muted"
            >
              <span className="flex min-w-0 items-center gap-1.5">
                <LeakDot tone={leakTone(p) ?? "warn"} />
                <span className="min-w-0 truncate font-mono text-small">{p.address}</span>
                {p.label && (
                  <span className="min-w-0 truncate text-meta text-muted-foreground">
                    {p.label}
                  </span>
                )}
                <span className="ml-auto shrink-0 pl-2 text-meta text-muted-foreground tabular-nums">
                  {p.last_offense ? listTime(p.last_offense) : ""}
                </span>
              </span>
              <span className="truncate text-small">
                <LeakCounts p={p} />
                {strangers.length > 0 && (
                  <span className="text-muted-foreground">
                    {" "}
                    · {m.leak_from({ domains: strangers.join(", ") })}
                  </span>
                )}
              </span>
            </button>
          </li>
        );
      })}
    </ul>
  );
}

// The detail shows this many offending messages; the rest are in the
// prefix's own list right below.
const shownOffenses = 3;

/**
 * Above the mail of a claimed prefix: whom it was given to, who DMARC
 * proved to send to it, and the latest mail that leaked or spoofed.
 * One line while nothing offended; unclaimed prefixes have no check.
 */
export function PrefixFacts({
  address,
  manage,
  onOpen,
}: {
  address: string;
  manage: boolean;
  onOpen: (message: string) => void;
}) {
  const ps = usePrefixes();
  const p = ps.data?.prefixes.find((x) => x.address === address);
  const tone = p && leakTone(p);
  const detail = usePrefixDetail(address, !!tone);
  const [editing, setEditing] = useState(false);
  const [counted, setCounted] = useState<CountedAs[]>([]);
  // Only claimed prefixes are listed here; any other has no leak check,
  // and claiming it is the decision on offer.
  if (!p) return ps.data && manage ? <UnclaimedFacts address={address} /> : null;
  const expects = p.expected_domains.length ? p.expected_domains.join(", ") : undefined;
  return (
    <section
      aria-label={m.leak_facts_label()}
      className={cn(
        "flex flex-col gap-1 rounded-md border border-system-border bg-system-surface px-2 py-1 text-small",
        tone === "bad" && "border-destructive/60",
      )}
    >
      <p className="flex min-w-0 flex-wrap items-center gap-x-2">
        <Signature speaker={system} />
        {expects ? (
          <span>
            <ParaglideMessage
              message={m.leak_given_to}
              inputs={{ domains: expects }}
              markup={markup}
            />
          </span>
        ) : manage ? (
          // The editor below does what `yuuka prefix expect` would.
          <span className="text-muted-foreground">{m.leak_given_to_nobody_ui()}</span>
        ) : (
          <span className="text-muted-foreground">
            <ParaglideMessage message={m.leak_given_to_nobody} markup={markup} />
          </span>
        )}
        {tone && <LeakCounts p={p} />}
        {manage && !editing && (
          <button
            type="button"
            className="text-muted-foreground underline underline-offset-2 hover:text-foreground"
            onClick={() => setEditing(true)}
          >
            {expects ? m.prefix_expect_edit() : m.prefix_expect_set()}
          </button>
        )}
      </p>
      {editing && (
        <ExpectEditor
          address={address}
          expected={p.expected_domains}
          onClose={(c) => {
            setEditing(false);
            if (c) setCounted(c);
          }}
        />
      )}
      <CountedAsNotes counted={counted} />
      {p.observed.length > 0 && (
        <p className="wrap-anywhere text-muted-foreground">
          {m.leak_proven_senders()}{" "}
          {p.observed.map((o, i) => (
            <span key={o.domain}>
              {i > 0 && ", "}
              <span className="whitespace-nowrap">
                <span className={cn(expects && !o.expected && "font-medium text-warning")}>
                  {o.domain}
                </span>{" "}
                {count(o.messages)}
              </span>
            </span>
          ))}
        </p>
      )}
      {tone && (
        <Offenses offenses={detail.data?.offenses} total={p.leaks + p.spoofs} onOpen={onOpen} />
      )}
      {manage && (
        <div className="border-t border-system-border pt-1">
          <UnclaimButton address={address} />
        </div>
      )}
    </section>
  );
}

/** Above the mail of a prefix nobody claimed: why its mail is downgraded, and Claim. */
function UnclaimedFacts({ address }: { address: string }) {
  return (
    <section
      aria-label={m.leak_facts_label()}
      className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-md border border-system-border bg-system-surface px-2 py-1 text-small"
    >
      <Signature speaker={system} />
      <span className="text-muted-foreground">{m.prefix_unclaimed_facts()}</span>
      <ClaimButton address={address} />
    </section>
  );
}

function Offenses({
  offenses,
  total,
  onOpen,
}: {
  offenses?: Offense[];
  total: number;
  onOpen: (message: string) => void;
}) {
  if (!offenses) return <Skeleton className="h-3.5 w-2/3" />;
  const more = total - Math.min(offenses.length, shownOffenses);
  return (
    <ul
      className="flex flex-col border-t border-system-border pt-1"
      aria-label={m.leak_offending_label()}
    >
      {offenses.slice(0, shownOffenses).map((o) => (
        <li key={o.message.id}>
          <button
            type="button"
            onClick={() => onOpen(o.message.id)}
            className="flex w-full min-w-0 items-baseline gap-1.5 rounded-sm text-left hover:bg-muted"
          >
            <span
              className={cn(
                "w-9 shrink-0 font-medium",
                o.verdict === "spoof" ? "text-destructive" : "text-warning",
              )}
            >
              {o.verdict}
            </span>
            <span className="w-24 shrink-0 truncate text-muted-foreground">
              {o.domain ?? m.leak_unproven()}
            </span>
            <span className="min-w-0 flex-1 truncate underline-offset-2 hover:underline">
              {o.message.subject || sender(o.message.from_name, o.message.from_addr)}
            </span>
            <span className="shrink-0 text-meta text-muted-foreground tabular-nums">
              {listTime(o.delivered_at)}
            </span>
          </button>
        </li>
      ))}
      {more > 0 && (
        <li className="text-meta text-muted-foreground">{m.leak_more_below({ n: count(more) })}</li>
      )}
    </ul>
  );
}
