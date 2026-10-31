import { useQueryClient } from "@tanstack/react-query";
import { ArchiveIcon, ArchiveRestoreIcon, ArrowRightLeftIcon, HistoryIcon, PlusIcon, WandSparklesIcon, XIcon } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { api, data } from "@/api/client";
import { invalidateAnnotated } from "@/api/queries";
import type { AnnotateResult, Annotation, AnnotationOutcome, Message, RuleAction } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Signature, system } from "@/components/voice";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";
import { RuleDrafter } from "@/rules/drafter";
import { LabelInput } from "@/rules/label-input";
import { ActionWords, LabelChip } from "@/rules/words";
import { ProblemLine } from "@/send/marks";
import { problem, type Abilities } from "@/send/outgoing";

import { History } from "./history";

// What a person does to a message in the reading pane (§8.5, §12.9):
// labels, archive, and moving it between the two zones. Each is one call
// through the server's single annotation core; what came of it is said
// by yuuka, overruled included, and can be undone. Moving a message
// between zones offers a rule for mail like it, and so does "Mail like
// this"; neither creates one by itself (§12.12).

interface Done {
  result: AnnotateResult;
  asked: Annotation[];
}

export function AnnotateBar({ message: msg, can, onOpen, onOpenRule }: { message: Message; can: Abilities; onOpen: (id: string) => void; onOpenRule?: (id: string) => void }) {
  const qc = useQueryClient();
  const [adding, setAdding] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const [done, setDone] = useState<Done>();
  const [offer, setOffer] = useState<RuleAction[]>();
  const [drafting, setDrafting] = useState<RuleAction[]>();
  const [history, setHistory] = useState(false);
  const toZone = msg.class === "machine" ? "human" : "machine";

  const annotate = async (asked: Annotation[]) => {
    setBusy(true);
    setErr(undefined);
    try {
      const result = await data(api.POST("/api/v1/messages/{id}/annotations", { params: { path: { id: msg.id } }, body: { actions: asked } }));
      setDone({ result, asked });
      invalidateAnnotated(qc);
      return result;
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };
  const toggleArchive = () => void annotate([{ kind: msg.archived ? "unarchive" : "archive" }]);
  const reclassify = async () => {
    const r = await annotate([{ kind: "classify", class: toZone }]);
    // A rule is offered, never made (§12.9): only when the move took.
    if (can.rules && r?.messages[0]?.outcomes[0]?.outcome === "applied") setOffer([{ kind: "classify", class: toZone }]);
  };

  // e archives or brings back the open message; l adds a label.
  const openLabel = () => setAdding(true);
  const keys = useRef({ toggleArchive, openLabel });
  useEffect(() => {
    keys.current = { toggleArchive, openLabel };
  });
  useEffect(() => {
    if (!can.annotate) return;
    function onKey(e: KeyboardEvent) {
      const t = e.target as HTMLElement;
      if (e.metaKey || e.ctrlKey || e.altKey || t.closest("input, textarea, select, [contenteditable]")) return;
      if (e.key === "e") keys.current.toggleArchive();
      else if (e.key === "l") {
        e.preventDefault();
        keys.current.openLabel();
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [can.annotate]);

  return (
    <section aria-label={m.annotate_label()} className="flex flex-col gap-1.5">
      <div className="flex flex-wrap items-center gap-1.5 text-small">
        {msg.labels.map((l) => (
          <LabelChip key={l} label={l}>
            {can.annotate && (
              <button type="button" aria-label={m.label_remove({ label: l })} className="text-muted-foreground hover:text-foreground" disabled={busy} onClick={() => void annotate([{ kind: "unlabel", label: l }])}>
                <XIcon className="size-3" />
              </button>
            )}
          </LabelChip>
        ))}
        {can.annotate &&
          (adding ? (
            <LabelInput
              autoFocus
              exclude={msg.labels}
              onPick={(l) => {
                setAdding(false);
                void annotate([{ kind: "label", label: l }]);
              }}
              onCancel={() => setAdding(false)}
            />
          ) : (
            <Button size="xs" variant="ghost" title={m.label_add_key()} onClick={() => setAdding(true)}>
              <PlusIcon data-icon="inline-start" />
              {m.label_add()}
            </Button>
          ))}
        <span className="ml-auto flex flex-wrap items-center gap-1">
          {can.annotate && (
            <>
              <Button size="xs" variant="outline" disabled={busy} title={msg.archived ? m.annotate_unarchive_key() : m.annotate_archive_key()} onClick={toggleArchive}>
                {msg.archived ? <ArchiveRestoreIcon data-icon="inline-start" /> : <ArchiveIcon data-icon="inline-start" />}
                {msg.archived ? m.annotate_unarchive() : m.annotate_archive()}
              </Button>
              <Button size="xs" variant="outline" disabled={busy} onClick={() => void reclassify()}>
                <ArrowRightLeftIcon data-icon="inline-start" />
                {toZone === "machine" ? m.annotate_to_notifications() : m.annotate_to_conversations()}
              </Button>
            </>
          )}
          {can.rules && (
            <Button size="xs" variant="outline" aria-expanded={!!drafting} onClick={() => setDrafting(drafting ? undefined : [])}>
              <WandSparklesIcon data-icon="inline-start" />
              {m.annotate_like_this()}
            </Button>
          )}
          <Button size="xs" variant="ghost" aria-expanded={history} onClick={() => setHistory((h) => !h)}>
            <HistoryIcon data-icon="inline-start" />
            {m.annotate_why()}
          </Button>
        </span>
      </div>
      {msg.archived && !done && <p className="text-small text-muted-foreground">{m.annotate_is_archived()}</p>}
      {done && <Outcomes key={done.result.action_id ?? ""} done={done} onUndone={() => setOffer(undefined)} />}
      {err !== undefined && <ProblemLine p={problem(err)} />}
      {offer && !drafting && (
        <div className="flex flex-wrap items-center gap-2 rounded-md border border-system-border bg-system-surface px-2 py-1 text-small">
          <Signature speaker={system} />
          <span>{offer[0]?.class === "machine" ? m.offer_moved_notifications() : m.offer_moved_conversations()}</span>
          <span className="ml-auto flex gap-1">
            <Button
              size="xs"
              onClick={() => {
                setDrafting(offer);
                setOffer(undefined);
              }}
            >
              {m.offer_draft()}
            </Button>
            <Button size="xs" variant="ghost" onClick={() => setOffer(undefined)}>
              {m.offer_not_now()}
            </Button>
          </span>
        </div>
      )}
      {drafting && <RuleDrafter messageId={msg.id} initialActions={drafting} onClose={() => setDrafting(undefined)} onOpen={onOpen} />}
      {history && <History message={msg} can={can} onOpenRule={onOpenRule} />}
    </section>
  );
}

/** One annotation's outcome in words: applied is the change itself, the rest say why not. */
export function outcomeText(o: AnnotationOutcome["outcome"]): string | undefined {
  switch (o) {
    case "applied":
      return undefined;
    case "unchanged":
      return m.outcome_unchanged();
    case "overruled":
      return m.outcome_overruled();
    case "labels_full":
      return m.outcome_labels_full();
  }
}

/**
 * What came of an annotation, in yuuka's voice, with undo while it is the
 * latest thing done here. overruled is a normal result: someone of higher
 * rank decided this value last (§8.5).
 */
export function Outcomes({ done, onUndone }: { done: Done | { result: AnnotateResult }; onUndone?: () => void }) {
  const qc = useQueryClient();
  const [undo, setUndo] = useState<{ reverted: number; superseded: number } | "busy">();
  const [err, setErr] = useState<unknown>();
  const outs = done.result.messages.flatMap((x) => x.outcomes);
  const id = done.result.action_id;
  const many = done.result.messages.length > 1;
  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 rounded-md border border-system-border bg-system-surface px-2 py-0.5 text-small">
      <Signature speaker={system} />
      {many && <span className="text-muted-foreground">{m.outcome_messages({ n: done.result.messages.length })}</span>}
      {dedupe(outs).map((o, i) => {
        const why = outcomeText(o.outcome);
        return (
          <span key={i} className={cn("inline-flex items-center gap-1", o.outcome === "overruled" || o.outcome === "labels_full" ? "text-warning" : "")}>
            <ActionWords a={o} />
            {why && <span className={o.outcome === "unchanged" ? "text-muted-foreground" : "font-medium"}>· {why}</span>}
          </span>
        );
      })}
      {id && undo === undefined && (
        <button
          type="button"
          className="ml-auto underline underline-offset-2"
          onClick={async () => {
            setUndo("busy");
            try {
              const r = await data(api.POST("/api/v1/actions/{id}/undo", { params: { path: { id } } }));
              setUndo(r);
              invalidateAnnotated(qc);
              onUndone?.();
            } catch (e) {
              setErr(e);
              setUndo(undefined);
            }
          }}
        >
          {m.undo()}
        </button>
      )}
      {undo && undo !== "busy" && <UndoResultText r={undo} />}
      {err !== undefined && <ProblemLine p={problem(err)} />}
    </div>
  );
}

function dedupe(os: AnnotationOutcome[]): AnnotationOutcome[] {
  const seen = new Set<string>();
  return os.filter((o) => {
    const k = `${o.kind} ${o.label ?? ""} ${o.class ?? ""} ${o.outcome}`;
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  });
}

export function UndoResultText({ r }: { r: { reverted: number; superseded: number } }) {
  return (
    <span className="text-muted-foreground">
      {m.undo_reverted({ n: String(r.reverted) })}
      {r.superseded > 0 && <span className="text-warning"> · {m.undo_superseded({ n: String(r.superseded) })}</span>}
    </span>
  );
}
