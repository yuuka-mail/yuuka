import { useState } from "react";

import { useLabels } from "@/api/queries";
import { count } from "@/lib/format";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";

/**
 * Why a label cannot be stored, or undefined when it can: the server's
 * rules (rules.ValidLabel), checked early so the person sees it while
 * typing. The server checks again.
 */
export function labelProblem(l: string): string | undefined {
  if (!l) return undefined;
  if ([...l].length > 64) return m.label_too_long();
  // oxlint-disable-next-line no-control-regex -- control characters are exactly what is refused
  if (/[\u0000-\u001f\u007f]/.test(l)) return m.label_control();
  if (l.startsWith("$") || l.startsWith("\\")) return m.label_reserved();
  return undefined;
}

/**
 * Typing a label, with the labels already in use offered as it is typed.
 * Labels are exact, case-sensitive strings (§8.5): "AWS" and "aws" are two
 * labels. So the one that differs only in case is pointed out, and the
 * existing spelling is one click away.
 */
export function LabelInput({
  onPick,
  onCancel,
  exclude = [],
  autoFocus,
  placeholder,
}: {
  onPick: (label: string) => void;
  onCancel?: () => void;
  /** Labels already there, not offered again. */
  exclude?: string[];
  autoFocus?: boolean;
  placeholder?: string;
}) {
  const labels = useLabels();
  const [text, setText] = useState("");
  const [hi, setHi] = useState(0);
  const t = text.trim();
  const all = labels.data?.labels ?? [];
  const lower = t.toLowerCase();
  const shown = all
    .filter((l) => !exclude.includes(l.label) && (!t || l.label.toLowerCase().includes(lower)))
    .slice(0, 8);
  const exact = all.some((l) => l.label === t);
  const nearly = !exact ? all.filter((l) => l.label !== t && l.label.toLowerCase() === lower) : [];
  const problem = labelProblem(t);
  const pick = (l: string) => {
    if (!l || labelProblem(l)) return;
    onPick(l);
    setText("");
    setHi(0);
  };
  return (
    <div className="relative flex min-w-0 flex-col gap-0.5">
      <input
        aria-label={m.label_input()}
        // oxlint-disable-next-line jsx-a11y/no-autofocus -- opened by a press of "Add label"
        autoFocus={autoFocus}
        value={text}
        placeholder={placeholder ?? m.label_placeholder()}
        spellCheck={false}
        autoComplete="off"
        aria-invalid={!!problem || undefined}
        onChange={(e) => {
          setText(e.target.value);
          setHi(0);
        }}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown") {
            e.preventDefault();
            setHi((i) => Math.min(i + 1, shown.length));
          } else if (e.key === "ArrowUp") {
            e.preventDefault();
            setHi((i) => Math.max(i - 1, 0));
          } else if (e.key === "Enter") {
            e.preventDefault();
            // 0 is what is typed; 1.. the offered labels.
            pick(hi > 0 ? (shown[hi - 1]?.label ?? t) : t);
          } else if (e.key === "Escape") {
            e.stopPropagation();
            onCancel?.();
          }
        }}
        className="h-6 w-44 min-w-0 rounded-md border border-input bg-background px-1.5 text-small outline-none focus-visible:border-ring aria-invalid:border-warning"
      />
      {problem && <span className="text-meta text-warning">{problem}</span>}
      {nearly.length > 0 && (
        <span className="text-meta text-warning">
          {m.label_case_differs()}{" "}
          {nearly.map((l) => (
            <button
              key={l.label}
              type="button"
              className="font-medium underline underline-offset-2"
              onClick={() => pick(l.label)}
            >
              {l.label}
            </button>
          ))}
        </span>
      )}
      {shown.length > 0 && (
        <ul
          role="listbox"
          aria-label={m.label_existing()}
          className="absolute top-7 left-0 z-20 w-56 rounded-md border bg-background p-1 shadow-md"
        >
          {shown.map((l, i) => (
            <li key={l.label} role="option" aria-selected={hi === i + 1}>
              <button
                type="button"
                // Keeps focus in the input, so Enter and the arrows still work.
                onMouseDown={(e) => e.preventDefault()}
                onClick={() => pick(l.label)}
                className={cn(
                  "flex h-6 w-full items-center gap-2 rounded-sm px-1.5 text-left text-small hover:bg-muted",
                  hi === i + 1 && "bg-muted",
                )}
              >
                <span className="min-w-0 truncate">{l.label}</span>
                <span className="ml-auto shrink-0 text-meta text-muted-foreground tabular-nums">
                  {count(l.messages)}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
