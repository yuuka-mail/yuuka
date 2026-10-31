import { CheckIcon, CopyIcon, ExternalLinkIcon } from "lucide-react";
import { useEffect, useState, type SyntheticEvent } from "react";

import type { Extracted } from "@/api/types";
import { cn } from "@/lib/utils";

import { openable, timeLeft } from "./extracted";

/**
 * The second line of a notification row with an extraction (§12.9): the
 * code, big and copyable, or the sign-in link as its host and "open",
 * then how long it lasts and the subject. The value is yuuka speaking
 * (§12.5): the system colour and square mark. An expired code turns
 * grey and stays. A code from a sender nobody proved is marked.
 *
 * Nothing here opens a link by itself: "open" is a plain link a person
 * presses, in a new tab, without a referrer.
 */
export function ExtractedLine({ x, verified, subject, mark = true }: { x: Extracted; verified?: boolean; subject?: string; mark?: boolean }) {
  const now = useNow(x.expires_at ? 15_000 : 0);
  const left = x.expires_at ? timeLeft(x.expires_at, now) : undefined;
  const expired = left?.expired ?? x.expired;
  const link = !x.code && x.link ? openable(x.link.value) : undefined;
  if (!x.code && !link) return null;
  const tone = expired ? "text-muted-foreground" : "text-system-foreground";
  return (
    <span className="flex min-w-0 items-center gap-1.5 text-ui">
      {mark && <span aria-hidden className={cn("size-1.5 shrink-0 rounded-[1px]", expired ? "bg-muted-foreground" : "bg-system")} />}
      {x.code ? (
        <>
          <span
            className={cn("shrink-0 font-mono text-body font-semibold tracking-wider tabular-nums", tone)}
            title={`Code found by yuuka in the ${x.code.source} (confidence ${x.code.confidence})`}
          >
            {x.code.value}
          </span>
          <CopyButton value={x.code.value} />
        </>
      ) : (
        link && (
          <a
            href={link.href}
            target="_blank"
            rel="noopener noreferrer"
            referrerPolicy="no-referrer"
            onClick={stop}
            onKeyDown={stop}
            className={cn("inline-flex min-w-0 shrink items-center gap-1 underline underline-offset-2", tone)}
            title={`Open ${link.href} in a new tab`}
          >
            <span className="shrink-0 font-medium">open</span>
            <span className="truncate font-mono text-meta">{link.host}</span>
            <ExternalLinkIcon aria-hidden className="size-3 shrink-0" />
          </a>
        )
      )}
      {left && <span className={cn("shrink-0 text-meta tabular-nums", expired ? "text-muted-foreground" : "text-system-foreground")}>{left.text}</span>}
      {verified === false && (
        <span className="shrink-0 text-meta font-medium text-warning" title="No DMARC pass from the trusted receiver: the sender is unproven">
          unverified
        </span>
      )}
      {subject !== undefined && <span className="min-w-0 truncate text-muted-foreground">{subject}</span>}
    </span>
  );
}

const stop = (e: SyntheticEvent) => e.stopPropagation();

function CopyButton({ value }: { value: string }) {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(false), 1500);
    return () => clearTimeout(t);
  }, [copied]);
  return (
    <button
      type="button"
      aria-label={copied ? "Copied" : `Copy ${value}`}
      title="Copy the code"
      onClick={(e) => {
        e.stopPropagation();
        void navigator.clipboard?.writeText(value).then(
          () => setCopied(true),
          () => undefined,
        );
      }}
      onKeyDown={stop}
      className="inline-flex size-6 shrink-0 items-center justify-center rounded-md text-muted-foreground outline-none hover:bg-background hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
    >
      {copied ? <CheckIcon className="size-3.5 text-system-foreground" /> : <CopyIcon className="size-3.5" />}
    </button>
  );
}

/** The current time, refreshed every ms milliseconds; never when ms is 0. */
function useNow(ms: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!ms) return;
    const t = setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(t);
  }, [ms]);
  return now;
}
