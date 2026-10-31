import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { api, data } from "@/api/client";
import { invalidateAnnotated } from "@/api/queries";
import type { RetroStats, RevertStats } from "@/api/types";
import { Button } from "@/components/ui/button";
import { count } from "@/lib/format";
import { m } from "@/paraglide/messages.js";
import { ProblemLine } from "@/send/marks";
import { problem } from "@/send/outgoing";

// Running a saved rule over mail already here, and taking back what it
// did (§8.5). Neither happens by itself: a rule runs on arriving mail,
// and only a person runs it backwards, after seeing the count of a dry
// run. Both are what `yuuka rule apply` and `yuuka rule revert` do.

type Step =
  | { s: "idle" }
  | { s: "counted"; n: RetroStats }
  | { s: "applied"; n: RetroStats }
  | { s: "confirm-revert" }
  | { s: "reverted"; n: RevertStats };

export function RetroActions({ id, enabled = true }: { id: string; enabled?: boolean }) {
  const qc = useQueryClient();
  const [step, setStep] = useState<Step>({ s: "idle" });
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const run = async (f: () => Promise<Step>) => {
    setBusy(true);
    setErr(undefined);
    try {
      setStep(await f());
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };
  const apply = (dry: boolean) =>
    run(async () => {
      const n = await data(
        api.POST("/api/v1/rules/{id}/apply", { params: { path: { id } }, body: { dry_run: dry } }),
      );
      if (!dry) invalidateAnnotated(qc);
      return dry ? { s: "counted", n } : { s: "applied", n };
    });
  const revert = () =>
    run(async () => {
      const n = await data(
        api.POST("/api/v1/rules/{id}/revert", { params: { path: { id } }, body: {} }),
      );
      invalidateAnnotated(qc);
      return { s: "reverted", n };
    });

  return (
    <div className="flex flex-col gap-1.5 text-small">
      <div className="flex flex-wrap items-center gap-2">
        {step.s === "counted" ? (
          <>
            <Button
              size="xs"
              disabled={busy || step.n.changed === 0}
              onClick={() => void apply(false)}
            >
              {m.rule_apply_n({ n: count(step.n.changed) })}
            </Button>
            <Button size="xs" variant="ghost" onClick={() => setStep({ s: "idle" })}>
              {m.rule_cancel()}
            </Button>
          </>
        ) : step.s === "confirm-revert" ? (
          <>
            <Button size="xs" variant="destructive" disabled={busy} onClick={() => void revert()}>
              {m.rule_revert_confirm()}
            </Button>
            <Button size="xs" variant="ghost" onClick={() => setStep({ s: "idle" })}>
              {m.rule_cancel()}
            </Button>
          </>
        ) : (
          <>
            <Button
              size="xs"
              variant="outline"
              disabled={busy || !enabled}
              title={enabled ? undefined : m.rule_apply_disabled()}
              onClick={() => void apply(true)}
            >
              {m.rule_apply_past()}
            </Button>
            <Button
              size="xs"
              variant="outline"
              disabled={busy}
              onClick={() => setStep({ s: "confirm-revert" })}
            >
              {m.rule_revert()}
            </Button>
          </>
        )}
      </div>
      {step.s === "counted" && (
        <p>
          {m.rule_apply_dry({
            matched: count(step.n.matched),
            messages: count(step.n.messages),
            changed: count(step.n.changed),
          })}
          {step.n.overruled > 0 && (
            <span className="text-muted-foreground">
              {" "}
              · {m.rule_preview_overruled({ overruled: count(step.n.overruled) })}
            </span>
          )}
        </p>
      )}
      {step.s === "applied" && (
        <p className="text-system-foreground">
          {m.rule_applied({ changed: count(step.n.changed), matched: count(step.n.matched) })}
        </p>
      )}
      {step.s === "confirm-revert" && <p className="text-warning">{m.rule_revert_cost()}</p>}
      {step.s === "reverted" && (
        <p className="text-system-foreground">
          {m.rule_reverted({ reverted: count(step.n.reverted) })}
          {step.n.superseded > 0 && (
            <span className="text-muted-foreground">
              {" "}
              · {m.undo_superseded({ n: count(step.n.superseded) })}
            </span>
          )}
        </p>
      )}
      {err !== undefined && <ProblemLine p={problem(err)} />}
    </div>
  );
}
