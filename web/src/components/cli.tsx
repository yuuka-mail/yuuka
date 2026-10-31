import { CheckIcon, CopyIcon } from "lucide-react";
import { useState, type ReactNode } from "react";

import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";

import { Signature, system } from "./voice";

/**
 * Where to do what the WebUI does not (§13.10, §12.12): yuuka saying it,
 * with the host command to paste. Issuing, raising and adding are done on
 * the host; a greyed-out button would only say "not here", this says where.
 */
export function HostCommand({ why, command, className }: { why: ReactNode; command: string; className?: string }) {
  return (
    <div className={cn("flex flex-col gap-1 rounded-md border border-system-border bg-system-surface px-2 py-1.5 text-small", className)}>
      <p className="flex flex-wrap items-baseline gap-x-2">
        <Signature speaker={system} />
        <span>{why}</span>
      </p>
      <CommandLine command={command} />
    </div>
  );
}

/** One command, as text to copy; never run, never a link. */
export function CommandLine({ command }: { command: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <span className="flex min-w-0 items-start gap-1">
      <code className="min-w-0 flex-1 rounded-sm bg-background/60 px-1.5 py-0.5 font-mono text-small wrap-anywhere">{command}</code>
      <button
        type="button"
        aria-label={copied ? m.cli_copied() : m.cli_copy()}
        title={copied ? m.cli_copied() : m.cli_copy()}
        onClick={() => {
          void navigator.clipboard
            ?.writeText(command)
            .then(() => {
              setCopied(true);
              setTimeout(() => setCopied(false), 1500);
            })
            .catch(() => undefined);
        }}
        className="inline-flex size-6 shrink-0 items-center justify-center rounded-md text-muted-foreground outline-none hover:bg-background hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
      >
        {copied ? <CheckIcon className="size-3.5 text-system-foreground" /> : <CopyIcon className="size-3.5" />}
      </button>
    </span>
  );
}

/**
 * The host command in a widening_requires_cli problem's detail (§13.10),
 * which ends "…; on the host: <command>". Undefined when it names none.
 */
export function hostCommandOf(detail: string): { why: string; command: string } | undefined {
  const i = detail.lastIndexOf("on the host: ");
  if (i < 0) return undefined;
  return { why: detail.slice(0, i).replace(/[;\s]+$/, ""), command: detail.slice(i + "on the host: ".length).trim() };
}
