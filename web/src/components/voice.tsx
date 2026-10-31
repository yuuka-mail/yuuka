import type { ReactNode } from "react";

import { cn } from "@/lib/utils";

/**
 * Who is speaking in a block (§11.1, §12.5). "system" is yuuka stating
 * facts the system established; "agent" is an agent's guess or proposal.
 * The two differ in colour, mark shape and signature, never only in
 * wording. Nothing in M1 speaks as an agent; the variant exists so the
 * tokens are in place before the first proposal card.
 */
export type Speaker = { kind: "system" } | { kind: "agent"; name: string };

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
};

export function Signature({ speaker, className }: { speaker: Speaker; className?: string }) {
  const s = styles[speaker.kind];
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-meta font-semibold tracking-wide", s.sign, className)}>
      <span aria-hidden className={cn("size-2", s.mark)} />
      {speaker.kind === "system" ? "yuuka" : speaker.name}
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
    <section className={cn("rounded-md border px-3 py-2 text-small", styles[speaker.kind].box, className)}>
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
