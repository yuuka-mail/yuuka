import { useQueryClient } from "@tanstack/react-query";
import { ArrowLeftIcon, TriangleAlertIcon } from "lucide-react";
import { useState } from "react";

import { api, data } from "@/api/client";
import { useOutboxEntry } from "@/api/queries";
import type { OutboxEntry, Resolution } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { system, Voice } from "@/components/voice";
import { cn } from "@/lib/utils";
import { useMessage } from "@/mail/reading-data";


import { ProblemLine } from "./marks";
import { OutboxFacts } from "./outbox-facts";
import { outboxState, problem, type Abilities } from "./outgoing";

/** One outbox row: its facts, its sent copy, and for unknown delivery the decision (§4.5). */
export function OutboxPane({
  id,
  can,
  onBack,
  onOpenMessage,
  onOpenDraft,
}: {
  id: string;
  can: Abilities;
  onBack: () => void;
  onOpenMessage: (id: string) => void;
  onOpenDraft: (id: string) => void;
}) {
  const q = useOutboxEntry(id);
  const e = q.data;
  const copy = useMessage(e?.message_id ?? "", !!e?.message_id);
  const s = e && outboxState(e.state);
  return (
    <article className="flex h-full min-h-0 flex-col">
      <div className="flex h-11 shrink-0 items-center border-b px-2 md:hidden">
        <Button variant="ghost" size="sm" onClick={onBack}>
          <ArrowLeftIcon data-icon="inline-start" />
          Back
        </Button>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto">
        {e && s ? (
          <div className="flex max-w-[56rem] flex-col gap-4 px-4 py-4 md:px-6">
            <header className="flex flex-col gap-1">
              <h1 className="text-title font-semibold break-words">
                {copy.data ? copy.data.subject || <span className="text-muted-foreground">(no subject)</span> : <span className="text-muted-foreground">Outbox {e.id}</span>}
              </h1>
              <p className="flex flex-wrap gap-x-3 text-small text-muted-foreground">
                {e.message_id && (
                  <button type="button" className="underline underline-offset-2 hover:text-foreground" onClick={() => onOpenMessage(e.message_id ?? "")}>
                    Open the sent copy
                  </button>
                )}
                {e.draft_id && (
                  <button type="button" className="underline underline-offset-2 hover:text-foreground" onClick={() => onOpenDraft(e.draft_id ?? "")}>
                    Open the draft
                  </button>
                )}
                <span className="font-mono">{e.id}</span>
              </p>
            </header>
            <Voice speaker={system} title="outbox" className={cn(e.state === "unknown" && "border-warning/60", e.state === "dead" && "border-destructive/60")}>
              <OutboxFacts e={e} />
            </Voice>
            {e.state === "unknown" && <Unknown e={e} canResolve={can.resolve} />}
          </div>
        ) : q.error ? (
          <p className="p-6">
            <ProblemLine p={problem(q.error)} />
          </p>
        ) : (
          <div className="flex flex-col gap-3 p-6">
            <Skeleton className="h-6 w-2/3" />
            <Skeleton className="mt-4 h-24 w-full" />
          </div>
        )}
      </div>
    </article>
  );
}

const resolutions: { r: Resolution; label: string; risk: string }[] = [
  {
    r: "sent",
    label: "It was sent",
    risk: "Records the unknown recipients as having received it. Choose this when you know it arrived: a reply, or the exit's own log.",
  },
  {
    r: "resend",
    label: "Send it again",
    risk: "Queues it again for the unknown recipients. If the first attempt did arrive, they get the message twice.",
  },
  {
    r: "abandon",
    label: "Give up",
    risk: "Records it as not sent. Nothing more is sent; if the first attempt did arrive, it stays arrived.",
  },
];

/**
 * §4.5: the exit may have accepted the message before the answer was
 * lost, and nothing yuuka holds can tell. A person decides; the decision
 * is recorded like `yuuka outbox resolve`. Without the right to decide
 * (a person, with submission:send) the choices are not offered at all.
 */
function Unknown({ e, canResolve }: { e: OutboxEntry; canResolve: boolean }) {
  const qc = useQueryClient();
  const [pick, setPick] = useState<Resolution>();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const apply = async () => {
    if (!pick || busy) return;
    setBusy(true);
    setErr(undefined);
    try {
      await data(api.POST("/api/v1/outbox/{id}/resolve", { params: { path: { id: e.id } }, body: { resolution: pick } }));
      setPick(undefined);
      void qc.invalidateQueries({ queryKey: ["get", "/api/v1/outbox"] });
      void qc.invalidateQueries({ queryKey: ["get", "/api/v1/outbox/{id}"] });
      void qc.invalidateQueries({ queryKey: ["get", "/api/v1/drafts"] });
      void qc.invalidateQueries({ queryKey: ["get", "/api/v1/messages"] });
    } catch (x) {
      setErr(x);
    } finally {
      setBusy(false);
    }
  };
  return (
    <section aria-label="Delivery unknown" className="flex flex-col gap-2 rounded-md border border-warning/60 px-3 py-2 text-small">
      <h2 className="flex items-center gap-1.5 font-semibold text-warning">
        <TriangleAlertIcon aria-hidden className="size-3.5" />
        May have been sent
      </h2>
      <p>
        The exit was handed the whole message, then the answer was lost. It may have been delivered or not; yuuka cannot tell, so it does not retry on its own.
      </p>
      {canResolve ? (
        <>
          <fieldset className="flex flex-col gap-1.5">
            <legend className="sr-only">Decide</legend>
            {resolutions.map((x) => (
              <label key={x.r} className={cn("flex cursor-pointer items-start gap-2 rounded-md border px-2 py-1.5 hover:bg-muted", pick === x.r && "border-foreground/40 bg-accent")}>
                <input type="radio" name={`resolve-${e.id}`} className="mt-0.5 accent-current" checked={pick === x.r} onChange={() => setPick(x.r)} />
                <span>
                  <span className="font-medium">{x.label}</span>
                  <span className="block text-muted-foreground">{x.risk}</span>
                </span>
              </label>
            ))}
          </fieldset>
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" variant={pick === "resend" ? "destructive" : "default"} disabled={!pick || busy} onClick={() => void apply()}>
              {busy ? "Recording…" : pick ? `${resolutions.find((x) => x.r === pick)?.label}` : "Choose one"}
            </Button>
            {pick === "resend" && <span className="text-warning">They may receive it twice.</span>}
            {err !== undefined && <ProblemLine p={problem(err)} />}
          </div>
        </>
      ) : (
        <p className="text-muted-foreground">Only a person signed in with submission:send can decide this.</p>
      )}
    </section>
  );
}
