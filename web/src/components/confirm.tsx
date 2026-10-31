import { useState, type ReactNode } from "react";

import { Button } from "@/components/ui/button";
import { m } from "@/paraglide/messages.js";
import { ProblemLine } from "@/send/marks";
import { problem } from "@/send/outgoing";

/**
 * An action that takes a second press, with what it costs written beside
 * the second one (§12.12): no modal, the way the outbox's decisions and a
 * rule's deletion ask. Errors are said by their code; onError may take
 * one over (a widening the host must make, say) and return true.
 */
export function ConfirmButton({
  label,
  confirmLabel,
  cost,
  onConfirm,
  onError,
  disabled,
}: {
  label: ReactNode;
  confirmLabel: ReactNode;
  cost: ReactNode;
  onConfirm: () => Promise<unknown>;
  onError?: (err: unknown) => boolean;
  disabled?: boolean;
}) {
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const run = async () => {
    setBusy(true);
    setErr(undefined);
    try {
      await onConfirm();
      setAsking(false);
    } catch (e) {
      if (!onError?.(e)) setErr(e);
      setAsking(false);
    } finally {
      setBusy(false);
    }
  };
  return (
    <span className="flex flex-col gap-1">
      <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
        {asking ? (
          <>
            <Button size="xs" variant="destructive" disabled={busy} onClick={() => void run()}>
              {confirmLabel}
            </Button>
            <Button size="xs" variant="ghost" onClick={() => setAsking(false)}>
              {m.rule_cancel()}
            </Button>
            <span className="text-small text-muted-foreground">{cost}</span>
          </>
        ) : (
          <Button size="xs" variant="outline" disabled={disabled} onClick={() => setAsking(true)}>
            {label}
          </Button>
        )}
      </span>
      {err !== undefined && <ProblemLine p={problem(err)} />}
    </span>
  );
}
