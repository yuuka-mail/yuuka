import { useState } from "react";

import type { RuleAction, RuleBody, RuleWarning } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";
import { ProblemLine } from "@/send/marks";
import { problem } from "@/send/outgoing";

import { ActionsEditor } from "./actions-editor";
import { actionsComplete, asMatch, fromEdit, newID, newLeaf, toEdit, type EditNode, type RuleMatch } from "./match";
import { MatchEditor } from "./match-editor";
import { RulePreviewBox, Warnings } from "./preview";
import { MatchWords } from "./words";

export interface RuleDraftForm {
  name: string;
  priority: number;
  enabled: boolean;
  match?: unknown;
  actions: RuleAction[];
}

function parseJSON(raw: string): RuleMatch | undefined {
  try {
    return asMatch(JSON.parse(raw));
  } catch {
    return undefined;
  }
}

/**
 * A whole rule: name, order, on or off, the conditions in the form (or
 * as JSON when the form cannot hold them), the actions, and what it would
 * do to the mail here now. Saving is the caller's: create, replace, or
 * accept a proposal with it.
 */
export function RuleEditor({
  initial,
  submitLabel,
  onSubmit,
  onOpen,
  showEnabled = true,
}: {
  initial: RuleDraftForm;
  submitLabel: string;
  /** Saves the rule; the warnings it came back with are shown. */
  onSubmit: (body: RuleBody) => Promise<RuleWarning[] | void>;
  onOpen?: (id: string) => void;
  showEnabled?: boolean;
}) {
  const start = asMatch(initial.match);
  // A new rule starts with one condition; a stored one the form cannot
  // hold (a field this version does not know, say) opens as JSON.
  const startTree: EditNode | undefined =
    initial.match === undefined ? { id: newID(), kind: "all", not: false, children: [newLeaf("from.addr")] } : start && toEdit(start);
  const [tree, setTree] = useState(startTree);
  const [raw, setRaw] = useState(() => JSON.stringify(initial.match ?? (startTree && fromEdit(startTree)), null, 2));
  const [json, setJSON] = useState(!tree);
  const [name, setName] = useState(initial.name);
  const [priority, setPriority] = useState(String(initial.priority));
  const [enabled, setEnabled] = useState(initial.enabled);
  const [actions, setActions] = useState(initial.actions);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const [warnings, setWarnings] = useState<RuleWarning[]>();

  const match = json ? parseJSON(raw) : tree && fromEdit(tree);
  const formable = !!(json && match && toEdit(match));
  const prio = Number.parseInt(priority, 10);
  const ready = !!match && actionsComplete(actions) && Number.isFinite(prio);

  const submit = async () => {
    if (!match || !ready) return;
    setBusy(true);
    setErr(undefined);
    try {
      const w = await onSubmit({ name: name.trim(), priority: prio, disabled: !enabled, match, actions });
      setWarnings(w ?? undefined);
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex flex-col gap-3 text-small">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1.5">
        <label className="flex min-w-0 items-center gap-1.5">
          <span className="text-muted-foreground">{m.rule_name()}</span>
          <Input value={name} onChange={(e) => setName(e.target.value)} maxLength={200} className="h-6 w-56 text-small" />
        </label>
        <label className="flex items-center gap-1.5" title={m.rule_priority_hint()}>
          <span className="text-muted-foreground">{m.rule_priority()}</span>
          <Input value={priority} inputMode="numeric" onChange={(e) => setPriority(e.target.value)} className="h-6 w-16 text-small tabular-nums" />
        </label>
        {showEnabled && (
          <label className="flex items-center gap-1.5">
            <input type="checkbox" className="accent-current" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
            {m.rule_enabled()}
          </label>
        )}
      </div>

      <section className="flex flex-col gap-1.5">
        <header className="flex items-center gap-2">
          <h3 className="font-medium">{m.rule_conditions()}</h3>
          <button
            type="button"
            className="ml-auto text-muted-foreground underline underline-offset-2 hover:text-foreground disabled:no-underline disabled:opacity-50"
            disabled={json ? !formable : false}
            onClick={() => {
              if (json) {
                if (match) setTree(toEdit(match));
                setJSON(false);
              } else {
                setRaw(JSON.stringify(match ?? {}, null, 2));
                setJSON(true);
              }
            }}
          >
            {json ? m.rule_edit_form() : m.rule_edit_json()}
          </button>
        </header>
        {json ? (
          <>
            {!tree && <p className="text-muted-foreground">{m.rule_json_only()}</p>}
            <textarea
              aria-label={m.rule_json()}
              value={raw}
              spellCheck={false}
              onChange={(e) => setRaw(e.target.value)}
              className={cn("min-h-40 w-full rounded-md border bg-background p-2 font-mono text-small outline-none focus-visible:border-ring", !match && "border-warning")}
            />
            {!match && <p className="text-warning">{m.rule_json_invalid()}</p>}
          </>
        ) : (
          tree && <MatchEditor value={tree} onChange={setTree} />
        )}
        {match && (
          <div className="rounded-md bg-muted/50 px-2 py-1">
            <span className="text-muted-foreground">{m.drafter_rule_reads()}</span>
            <MatchWords match={match} />
          </div>
        )}
      </section>

      <section className="flex flex-col gap-1.5">
        <h3 className="font-medium">{m.drafter_then()}</h3>
        <ActionsEditor value={actions} onChange={setActions} />
      </section>

      <RulePreviewBox match={match} actions={actions} onOpen={onOpen} />

      <div className="flex flex-wrap items-center gap-2">
        <Button size="sm" disabled={!ready || busy} onClick={() => void submit()}>
          {submitLabel}
        </Button>
        {err !== undefined && <ProblemLine p={problem(err)} />}
      </div>
      {warnings && <Warnings warnings={warnings} />}
    </div>
  );
}
