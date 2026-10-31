import { useQueryClient } from "@tanstack/react-query";
import { XIcon } from "lucide-react";
import { useState } from "react";

import { api, data } from "@/api/client";
import { invalidateAnnotated, useRuleDraft } from "@/api/queries";
import type { RuleAction, SavedRule } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Signature, system } from "@/components/voice";
import { count } from "@/lib/format";
import { m } from "@/paraglide/messages.js";
import { ProblemLine } from "@/send/marks";
import { problem } from "@/send/outgoing";

import { ActionsEditor } from "./actions-editor";
import { actionsComplete, allOf, asMatch, isLeaf, type Leaf } from "./match";
import { RulePreviewBox, Warnings } from "./preview";
import { RetroActions } from "./retro";
import { LeafWords, MatchWords } from "./words";

// The sender writes the subject and the body; a rule drafted from one
// message never generalises from them (§8.5). The server offers no such
// candidate; this is a second guard, not a feature.
const senderWritten = new Set(["subject", "body"]);

/**
 * "Mail like this from now on" (§8.5, §12.12): conditions drawn from this
 * message's sender, List-Id and receiving prefix, each with how much
 * mail it alone matches, the ticked ones in plain words, the actions, and
 * what the whole would do to the mail here now. Nothing is created until
 * the person saves; running it over past mail is a separate, counted step.
 *
 * With proposal, saving accepts that proposal with the rule (one
 * transaction on the server) instead of creating it on its own.
 */
export function RuleDrafter({
  messageId,
  initialActions,
  proposal,
  onClose,
  onOpen,
  onDone,
}: {
  messageId: string;
  initialActions: RuleAction[];
  proposal?: string;
  onClose: () => void;
  onOpen?: (id: string) => void;
  onDone?: () => void;
}) {
  const qc = useQueryClient();
  const draft = useRuleDraft(messageId);
  const cands = (draft.data?.candidates ?? []).filter((c) => !senderWritten.has(c.condition.field));
  const [ticked, setTicked] = useState<boolean[]>();
  const sel = ticked ?? cands.map((c) => c.selected);
  const [actions, setActions] = useState(initialActions);
  const [name, setName] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const [saved, setSaved] = useState<SavedRule>();

  const conds: Leaf[] = cands
    .filter((_, i) => sel[i])
    .flatMap((c) => {
      const leaf = asMatch(c.condition);
      return leaf && isLeaf(leaf) ? [leaf] : [];
    });
  const match = allOf(conds);
  const ready = !!match && actionsComplete(actions);
  const defaultName = conds[0]
    ? String(Array.isArray(conds[0].value) ? conds[0].value.join(", ") : conds[0].value)
    : "";

  const save = async () => {
    if (!match || !ready) return;
    setBusy(true);
    setErr(undefined);
    const rule = { name: (name ?? defaultName).trim(), match, actions };
    try {
      if (proposal) {
        const r = await data(
          api.POST("/api/v1/proposals/{id}/accept", {
            params: { path: { id: proposal } },
            body: { rule },
          }),
        );
        setSaved(r.rule);
      } else {
        setSaved(await data(api.POST("/api/v1/rules", { body: rule })));
      }
      invalidateAnnotated(qc);
      onDone?.();
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section
      aria-label={m.drafter_label()}
      className="flex flex-col gap-2 rounded-md border px-3 py-2 text-small"
    >
      <header className="flex items-center gap-2">
        <h2 className="font-semibold">{proposal ? m.drafter_title_accept() : m.drafter_title()}</h2>
        <button
          type="button"
          aria-label={m.rule_cancel()}
          className="ml-auto text-muted-foreground hover:text-foreground"
          onClick={onClose}
        >
          <XIcon className="size-3.5" />
        </button>
      </header>
      {saved ? (
        <div className="flex flex-col gap-1.5 rounded-md border border-system-border bg-system-surface px-2 py-1.5">
          <p className="flex flex-wrap items-center gap-2">
            <Signature speaker={system} />
            <span>{proposal ? m.drafter_accepted() : m.drafter_saved()}</span>
          </p>
          <Warnings warnings={saved.warnings} />
          <RetroActions id={saved.id} />
        </div>
      ) : draft.error ? (
        <ProblemLine p={problem(draft.error)} />
      ) : !draft.data ? (
        <Skeleton className="h-16 w-full" />
      ) : (
        <>
          <fieldset className="flex flex-col gap-0.5">
            <legend className="mb-0.5 text-muted-foreground">{m.drafter_when()}</legend>
            {cands.map((c, i) => {
              const leaf = asMatch(c.condition);
              return (
                <label
                  key={i}
                  className="flex min-w-0 cursor-pointer items-baseline gap-2 rounded-sm px-1 hover:bg-muted"
                >
                  <input
                    type="checkbox"
                    className="shrink-0 self-center accent-current"
                    checked={!!sel[i]}
                    onChange={(e) => setTicked(sel.map((x, j) => (j === i ? e.target.checked : x)))}
                  />
                  <span className="min-w-0 flex-1">
                    {leaf && isLeaf(leaf) ? <LeafWords leaf={leaf} /> : null}
                  </span>
                  <span className="shrink-0 text-meta text-muted-foreground tabular-nums">
                    {m.drafter_alone({ n: count(c.matches) })}
                  </span>
                </label>
              );
            })}
            {cands.length === 0 && (
              <p className="text-muted-foreground">{m.drafter_no_candidates()}</p>
            )}
          </fieldset>
          {match ? (
            <div className="flex flex-col gap-0.5">
              <span className="text-muted-foreground">{m.drafter_rule_reads()}</span>
              <MatchWords match={match} />
            </div>
          ) : (
            <p className="text-warning">{m.drafter_tick_one()}</p>
          )}
          <div className="flex flex-col gap-0.5">
            <span className="text-muted-foreground">{m.drafter_then()}</span>
            <ActionsEditor value={actions} onChange={setActions} />
          </div>
          <RulePreviewBox match={match} actions={actions} onOpen={onOpen} />
          <div className="flex flex-wrap items-center gap-2">
            <label className="flex min-w-0 items-center gap-1.5">
              <span className="shrink-0 text-muted-foreground">{m.rule_name()}</span>
              <Input
                value={name ?? defaultName}
                onChange={(e) => setName(e.target.value)}
                maxLength={200}
                className="h-6 w-56 text-small"
              />
            </label>
            <Button size="xs" disabled={!ready || busy} onClick={() => void save()}>
              {proposal ? m.drafter_accept_save() : m.drafter_save()}
            </Button>
            <span className="text-meta text-muted-foreground">{m.drafter_not_past()}</span>
          </div>
          {err !== undefined && <ProblemLine p={problem(err)} />}
        </>
      )}
    </section>
  );
}
