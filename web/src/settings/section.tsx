import type { ReactNode } from "react";

import { CommandLine } from "@/components/cli";
import { Button } from "@/components/ui/button";
import { Signature, system } from "@/components/voice";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";

/** The heading of a settings section, with room for a control at its right. */
export function SectionHeader({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <header className="flex flex-wrap items-center gap-2">
      <h2 className="text-title font-semibold">{title}</h2>
      {children && <span className="ml-auto flex items-center gap-2">{children}</span>}
    </header>
  );
}

/** "label value" pairs of a row's second line, the value in the meta size. */
export function Dates({ items }: { items: [string, string | undefined][] }) {
  return (
    <span className="flex flex-wrap gap-x-3 text-meta text-muted-foreground tabular-nums">
      {items
        .filter(([, v]) => v)
        .map(([k, v]) => (
          <span key={k}>
            {k} {v}
          </span>
        ))}
    </span>
  );
}

/** The frame of a form that widens who may do what (§13.10): what it asks before it runs. */
export function WideningForm({
  legend,
  children,
  onSubmit,
  className,
}: {
  legend: string;
  children: ReactNode;
  onSubmit: () => void;
  className?: string;
}) {
  return (
    <form
      className={cn("flex flex-col gap-2 rounded-md border px-3 py-2.5 text-small", className)}
      onSubmit={(e) => {
        e.preventDefault();
        onSubmit();
      }}
    >
      <p className="flex flex-wrap items-baseline gap-x-2">
        <span className="font-medium">{legend}</span>
        <span className="text-muted-foreground">{m.widen_asks_passkey()}</span>
      </p>
      {children}
    </form>
  );
}

/** One labelled field of a settings form: the label above, a hint below. */
export function Field({
  label,
  hint,
  children,
  className,
}: {
  label: string;
  hint?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <label className={cn("flex min-w-0 flex-col gap-0.5", className)}>
      <span className="text-muted-foreground">{label}</span>
      {children}
      {hint && <span className="text-meta text-muted-foreground">{hint}</span>}
    </label>
  );
}

export const inputClass =
  "h-7 w-full min-w-0 rounded-md border border-input bg-background px-2 font-mono text-small outline-none focus-visible:border-ring";
export const selectClass =
  "h-7 shrink-0 rounded-md border border-input bg-background px-1.5 text-small outline-none focus-visible:border-ring";
const principalKinds = ["human", "agent", "service"] as const;
type PrincipalKind = (typeof principalKinds)[number];

/** kind:name of a principal, as two controls; kinds limits which kinds are offered. */
export function PrincipalInput({
  value,
  onChange,
  kinds = principalKinds,
  label,
}: {
  value: string;
  onChange: (v: string) => void;
  kinds?: readonly PrincipalKind[];
  label: string;
}) {
  const i = value.indexOf(":");
  const kind = (i < 0 ? kinds[0] : value.slice(0, i)) as PrincipalKind;
  const name = i < 0 ? value : value.slice(i + 1);
  return (
    <span className="flex min-w-0 flex-col gap-0.5">
      <span className="text-muted-foreground">{label}</span>
      <span className="flex min-w-0 items-center gap-1">
        <select
          aria-label={m.principal_kind()}
          className={selectClass}
          value={kind}
          onChange={(e) => onChange(`${e.target.value}:${name}`)}
        >
          {kinds.map((k) => (
            <option key={k} value={k}>
              {k}
            </option>
          ))}
        </select>
        <span className="text-muted-foreground">:</span>
        <input
          aria-label={m.principal_name()}
          className={inputClass}
          value={name}
          placeholder={kind === "human" ? "operator" : "arisu"}
          onChange={(e) => onChange(`${kind}:${e.target.value.trim()}`)}
        />
      </span>
    </span>
  );
}

/** Whether s is kind:name with a name the server accepts (store.ValidName). */
export function validPrincipal(s: string): boolean {
  return /^(human|agent|service):[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(s);
}

/** A secret shown this once (§13.3): copied from here, or lost. */
export function SecretOnce({
  lines,
  onDone,
}: {
  lines: { label: string; value: string }[];
  onDone: () => void;
}) {
  return (
    <div className="flex flex-col gap-1.5 rounded-md border border-system-border bg-system-surface px-2 py-2 text-small">
      <p className="flex flex-wrap items-baseline gap-x-2">
        <Signature speaker={system} />
        <span className="font-medium">{m.secret_once()}</span>
      </p>
      {lines.map((l) => (
        <span key={l.label} className="flex flex-col gap-0.5">
          <span className="text-muted-foreground">{l.label}</span>
          <CommandLine command={l.value} />
        </span>
      ))}
      <span>
        <Button size="xs" variant="outline" onClick={onDone}>
          {m.secret_saved()}
        </Button>
      </span>
    </div>
  );
}
