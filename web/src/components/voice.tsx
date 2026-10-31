import type { ReactNode } from "react";

import { cn } from "@/lib/utils";

/**
 * Who is speaking in a block (§11.1, §12.5). "system" is yuuka stating
 * facts the system established; "agent" is an agent's guess or proposal;
 * "observation" is a scorer's estimate (§15), probabilities that act on
 * nothing, signed with the scorer and model because no principal stands
 * behind them. The three differ in colour, mark shape and signature,
 * never only in wording.
 */
export type Speaker =
  | { kind: "system" }
  | { kind: "agent"; name: string }
  | { kind: "observation"; scorer: string; model: string };

const styles = {
  system: {
    box: "border-system-border bg-system-surface",
    mark: "rounded-[2px] bg-system",
    sign: "text-system-foreground",
  },
  agent: {
    box: "border-agent-border bg-agent-surface",
    mark: "rounded-full bg-agent",
    sign: "text-agent-foreground",
  },
  // A diamond: neither yuuka's square nor an agent's circle.
  observation: {
    box: "border-observation-border bg-observation-surface",
    mark: "m-px size-1.5 rotate-45 rounded-[1px] bg-observation",
    sign: "text-observation-foreground",
  },
};

/** The mark alone, for a row too small for a signature (a list row). */
export function Mark({ kind, className }: { kind: Speaker["kind"]; className?: string }) {
  return (
    <span
      aria-hidden
      className={cn("inline-block size-2 shrink-0", styles[kind].mark, className)}
    />
  );
}

export function Signature({ speaker, className }: { speaker: Speaker; className?: string }) {
  const s = styles[speaker.kind];
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 text-meta font-semibold tracking-wide",
        s.sign,
        className,
      )}
    >
      <Mark kind={speaker.kind} />
      {speaker.kind === "system" ? (
        "yuuka"
      ) : speaker.kind === "agent" ? (
        speaker.name
      ) : (
        <>
          <span className="font-mono">{speaker.scorer}</span>
          {speaker.model && (
            <span className="font-mono font-normal tracking-normal">· {speaker.model}</span>
          )}
        </>
      )}
    </span>
  );
}

/** A block of text attributed to a speaker; action sits at the right of its header. */
export function Voice({
  speaker,
  title,
  action,
  children,
  className,
}: {
  speaker: Speaker;
  title?: ReactNode;
  action?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <section
      className={cn("rounded-md border px-3 py-2 text-small", styles[speaker.kind].box, className)}
    >
      <header className="mb-1 flex items-center gap-2">
        <Signature speaker={speaker} />
        {title && <span className="text-muted-foreground">{title}</span>}
        {action && <span className="ml-auto">{action}</span>}
      </header>
      {children}
    </section>
  );
}

export const system: Speaker = { kind: "system" };
