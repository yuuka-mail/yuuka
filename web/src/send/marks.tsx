import { SendIcon } from "lucide-react";

import type { Actor, HoldReason, OutboxState } from "@/api/types";
import { cn } from "@/lib/utils";

import { holdLabel, outboxState, principal, type problem, type Tone } from "./outgoing";

// The small inline marks of outgoing mail in lists and headers. Outbox
// states and hold reasons are yuuka's facts, so they take the system
// voice's colour and square mark; an author who is not a person takes
// the agent voice's round one (§12.5).

export const toneText: Record<Tone, string> = {
  ok: "text-system-foreground",
  moving: "text-muted-foreground",
  warn: "text-warning font-medium",
  bad: "text-destructive font-medium",
  plain: "text-muted-foreground",
};

/** Where outgoing mail stands, e.g. "↗ queued", in a row of a list. */
export function OutgoingMark({ state, className }: { state: OutboxState | string; className?: string }) {
  const s = outboxState(state);
  return (
    <span className={cn("inline-flex shrink-0 items-center gap-1 text-meta", toneText[s.tone], className)} title={`outbox: ${s.label}`}>
      <SendIcon aria-hidden className="size-3" />
      {s.label}
    </span>
  );
}

/** Why a draft stopped short of the outbox, said by yuuka. */
export function HeldMark({ reason, className }: { reason?: HoldReason; className?: string }) {
  return (
    <span className={cn("inline-flex shrink-0 items-center gap-1 text-meta text-system-foreground", className)}>
      <span aria-hidden className="size-1.5 rounded-[1px] bg-system" />
      held · {holdLabel(reason)}
    </span>
  );
}

/** An API error, by its stable code (§5.1), with the detail where it is specific. */
export function ProblemLine({ p }: { p: ReturnType<typeof problem> }) {
  return (
    <span className="text-small text-destructive">
      {p.text}
      {p.detail && <span className="text-foreground/80"> · {p.detail}</span>}
      {p.code && <span className="ml-1.5 font-mono text-meta text-muted-foreground">{p.code}</span>}
    </span>
  );
}

/** Who wrote a draft, when it was not a person: in the agent's voice. */
export function AuthorMark({ author, className }: { author: Actor; className?: string }) {
  return (
    <span className={cn("inline-flex shrink-0 items-center gap-1 text-meta font-semibold text-agent-foreground", className)}>
      <span aria-hidden className="size-1.5 rounded-full bg-agent" />
      {author.kind === "agent" ? author.name : principal(author)}
    </span>
  );
}
