import { useQueryClient } from "@tanstack/react-query";
import { ArrowLeftIcon, TriangleAlertIcon } from "lucide-react";
import { useState } from "react";

import { api, data } from "@/api/client";
import { useOutboxEntry } from "@/api/queries";
import type { OutboxEntry, Resolution, SessionInfo } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { system, Voice } from "@/components/voice";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";

import { ProblemLine } from "./marks";
import { OutboxFacts } from "./outbox-facts";
import { outboxState, principal, problem, recallable, type Abilities } from "./outgoing";
import { HoldUntil, Recall, ReviewVerdict } from "./review";

/** One outbox row: its facts, its sent copy, and for unknown delivery the decision (§4.5). */
export function OutboxPane({
  id,
  session,
  can,
  onBack,
  onOpenMessage,
  onOpenDraft,
}: {
  id: string;
  session: SessionInfo;
  can: Abilities;
  onBack: () => void;
  onOpenMessage: (id: string) => void;
  onOpenDraft: (id: string) => void;
}) {
  const q = useOutboxEntry(id);
  const e = q.data;
  const [recalled, setRecalled] = useState(false);
  const s = e && outboxState(e.state);
  return (
    <article className="flex h-full min-h-0 flex-col">
      <div className="flex h-11 shrink-0 items-center border-b px-2 md:hidden">
        <Button variant="ghost" size="sm" onClick={onBack}>
          <ArrowLeftIcon data-icon="inline-start" />
          {m.send_back()}
        </Button>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto">
        {e && s ? (
          <div className="flex max-w-[56rem] flex-col gap-4 px-4 py-4 md:px-6">
            <header className="flex flex-col gap-1">
              <h1 className="text-title font-semibold break-words">
                {e.subject || <span className="text-muted-foreground">{e.message_id ? m.send_no_subject() : m.send_outbox_row_title({ id: e.id })}</span>}
              </h1>
              <p className="flex flex-wrap gap-x-3 text-small text-muted-foreground">
                {e.message_id && (
                  <button type="button" className="underline underline-offset-2 hover:text-foreground" onClick={() => onOpenMessage(e.message_id ?? "")}>
                    {m.send_open_sent_copy()}
                  </button>
                )}
                {e.draft_id && (
                  <button type="button" className="underline underline-offset-2 hover:text-foreground" onClick={() => onOpenDraft(e.draft_id ?? "")}>
                    {m.send_open_draft()}
                  </button>
                )}
                <span className="font-mono">{e.id}</span>
              </p>
            </header>
            {/* First while it can still be stopped; kept after a recall, which
                refetches the row as withdrawn, so what the recall did stays in view. */}
            {(e.state === "held" || recalled) && (
              <Held e={e} canRecall={recallable(e, session, can) || recalled} onRecalled={() => setRecalled(true)} onOpenMessage={onOpenMessage} onOpenDraft={onOpenDraft} />
            )}
            <Voice speaker={system} title={m.send_outbox_voice_title()} className={cn(e.state === "unknown" && "border-warning/60", e.state === "dead" && "border-destructive/60")}>
              <OutboxFacts e={e} />
            </Voice>
            {e.state !== "held" && !recalled && e.review && (
              <Voice speaker={system} title={m.review_title()}>
                <ReviewVerdict review={e.review} onOpenMessage={onOpenMessage} />
              </Voice>
            )}
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

/**
 * Mail the review let through, waiting out its recall window (§8.9):
 * when it goes, what each check found, and Recall for whoever may.
 */
function Held({
  e,
  canRecall,
  onRecalled,
  onOpenMessage,
  onOpenDraft,
}: {
  e: OutboxEntry;
  canRecall: boolean;
  onRecalled: () => void;
  onOpenMessage: (id: string) => void;
  onOpenDraft: (id: string) => void;
}) {
  return (
    <Voice speaker={system} title={e.state === "held" ? m.review_held_title() : m.review_title()}>
      <div className="flex flex-col gap-1.5">
        {e.state === "held" && e.hold_until && (
          <p>
            <HoldUntil until={e.hold_until} />
          </p>
        )}
        {e.review && <ReviewVerdict review={e.review} onOpenMessage={onOpenMessage} />}
        {canRecall ? <Recall id={e.id} identity={e.identity} submitter={principal(e.submitted_by)} onRecalled={onRecalled} onOpenDraft={onOpenDraft} /> :<p className="text-muted-foreground">{m.review_recall_not_yours({ scope: "submission:send" })}</p>}
      </div>
    </Voice>
  );
}

// A function, not a constant: the words are the current locale's.
const resolutions = (): { r: Resolution; label: string; risk: string }[] => [
  { r: "sent", label: m.send_resolve_sent(), risk: m.send_resolve_sent_risk() },
  { r: "resend", label: m.send_resolve_resend(), risk: m.send_resolve_resend_risk() },
  { r: "abandon", label: m.send_resolve_abandon(), risk: m.send_resolve_abandon_risk() },
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
    <section aria-label={m.send_unknown_label()} className="flex flex-col gap-2 rounded-md border border-warning/60 px-3 py-2 text-small">
      <h2 className="flex items-center gap-1.5 font-semibold text-warning">
        <TriangleAlertIcon aria-hidden className="size-3.5" />
        {m.send_unknown_title()}
      </h2>
      <p>{m.send_unknown_body()}</p>
      {canResolve ? (
        <>
          <fieldset className="flex flex-col gap-1.5">
            <legend className="sr-only">{m.send_unknown_decide()}</legend>
            {resolutions().map((x) => (
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
              {busy ? m.send_recording() : pick ? `${resolutions().find((x) => x.r === pick)?.label}` : m.send_choose_one()}
            </Button>
            {pick === "resend" && <span className="text-warning">{m.send_resend_twice()}</span>}
            {err !== undefined && <ProblemLine p={problem(err)} />}
          </div>
        </>
      ) : (
        <p className="text-muted-foreground">{m.send_unknown_only_person({ scope: "submission:send" })}</p>
      )}
    </section>
  );
}
