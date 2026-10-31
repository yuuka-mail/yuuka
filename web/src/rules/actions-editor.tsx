import { XIcon } from "lucide-react";
import { useState } from "react";

import type { RuleAction } from "@/api/types";
import { m } from "@/paraglide/messages.js";

import { LabelInput } from "./label-input";
import { LabelChip, notifyName } from "./words";

// A rule's actions as a small form rather than a list: labels add up, the
// rest is one value each (§8.5). Annotations only; there is nothing here
// that sends, forwards or deletes, and the server refuses it anyway.

interface Parts {
  labels: string[];
  classify?: "machine" | "human";
  archive: boolean;
  notify?: "off" | "silent" | "normal";
}

function split(as: RuleAction[]): Parts {
  const p: Parts = { labels: [], archive: false };
  for (const a of as) {
    if (a.kind === "label" && a.label) p.labels.push(a.label);
    else if (a.kind === "classify") p.classify = a.class;
    else if (a.kind === "archive") p.archive = true;
    else if (a.kind === "notify") p.notify = a.level;
  }
  return p;
}

function join(p: Parts): RuleAction[] {
  const out: RuleAction[] = p.labels.map((label) => ({ kind: "label", label }));
  if (p.classify) out.push({ kind: "classify", class: p.classify });
  if (p.archive) out.push({ kind: "archive" });
  if (p.notify) out.push({ kind: "notify", level: p.notify });
  return out;
}

const selectClass = "h-6 rounded-md border border-input bg-background px-1 text-small outline-none focus-visible:border-ring";

export function ActionsEditor({ value, onChange }: { value: RuleAction[]; onChange: (as: RuleAction[]) => void }) {
  const p = split(value);
  const set = (patch: Partial<Parts>) => onChange(join({ ...p, ...patch }));
  const [adding, setAdding] = useState(false);
  return (
    <div className="flex flex-col gap-1.5 text-small">
      <div className="flex flex-wrap items-center gap-1.5">
        <span className="w-24 shrink-0 text-muted-foreground">{m.rule_actions_labels()}</span>
        {p.labels.map((l) => (
          <LabelChip key={l} label={l}>
            <button type="button" aria-label={m.label_remove({ label: l })} className="text-muted-foreground hover:text-foreground" onClick={() => set({ labels: p.labels.filter((x) => x !== l) })}>
              <XIcon className="size-3" />
            </button>
          </LabelChip>
        ))}
        {adding ? (
          <LabelInput
            autoFocus
            exclude={p.labels}
            onPick={(l) => {
              set({ labels: [...p.labels, l] });
              setAdding(false);
            }}
            onCancel={() => setAdding(false)}
          />
        ) : (
          <button type="button" className="text-muted-foreground underline underline-offset-2 hover:text-foreground" onClick={() => setAdding(true)}>
            {m.label_add()}
          </button>
        )}
      </div>
      <label className="flex items-center gap-1.5">
        <span className="w-24 shrink-0 text-muted-foreground">{m.rule_actions_zone()}</span>
        <select className={selectClass} value={p.classify ?? ""} onChange={(e) => set({ classify: (e.target.value || undefined) as Parts["classify"] })}>
          <option value="">{m.rule_actions_keep()}</option>
          <option value="machine">{m.area_notifications()}</option>
          <option value="human">{m.area_conversations()}</option>
        </select>
      </label>
      <label className="flex items-center gap-1.5">
        <span className="w-24 shrink-0 text-muted-foreground">{m.rule_actions_archive()}</span>
        <input type="checkbox" className="accent-current" checked={p.archive} onChange={(e) => set({ archive: e.target.checked })} />
      </label>
      <label className="flex items-center gap-1.5">
        <span className="w-24 shrink-0 text-muted-foreground">{m.rule_actions_notify()}</span>
        <select className={selectClass} value={p.notify ?? ""} onChange={(e) => set({ notify: (e.target.value || undefined) as Parts["notify"] })}>
          <option value="">{m.rule_actions_keep()}</option>
          {(["off", "silent", "normal"] as const).map((l) => (
            <option key={l} value={l}>
              {notifyName(l)}
            </option>
          ))}
        </select>
      </label>
    </div>
  );
}
