import { ParaglideMessage } from "@inlang/paraglide-js-react";
import { CircleHelpIcon, SearchIcon, XIcon } from "lucide-react";
import { useEffect, useId, useLayoutEffect, useRef, useState } from "react";

import { useLabels, usePrefixes } from "@/api/queries";
import type { TriageReport } from "@/api/types";
import { markup } from "@/components/markup";
import { Input } from "@/components/ui/input";
import { count } from "@/lib/format";
import { cn } from "@/lib/utils";
import { m as msg } from "@/paraglide/messages.js";

import {
  append,
  matching,
  pick,
  valueAt,
  type Choice,
  type Completable,
  type ValueAt,
} from "./search";
import { byQuestion, leanLabel } from "./triage";

/**
 * The search box (§16.5): its text is the whole query. Searching from a
 * view starts with that view's scope written in the box; values of the
 * operators that have a known set are offered as they are typed; the
 * operators are one press away.
 */
export function SearchBox({
  ref,
  value,
  scope,
  bcc,
  triage,
  onSearch,
}: {
  ref: React.RefObject<HTMLInputElement | null>;
  /** The query in the URL. */
  value: string;
  /** The current view's scope as query text, prefilled on focus. */
  scope?: string;
  /** Whether bcc: is open to this session (delivery:read). */
  bcc: boolean;
  /** The scorer's report: triage: is offered only with one (§15). */
  triage?: TriageReport;
  onSearch: (q: string) => void;
}) {
  const [text, setText] = useState(value);
  const [lastValue, setLastValue] = useState(value);
  // The scope alone is the view itself: nothing to search for yet.
  const query = (t: string) => (t.trim() === scope ? "" : t.trim());
  // Follow the URL when it changes underneath (Back, a prefix click), not
  // when it changes to what the box already says: the scope left in it
  // after deleting the rest stays.
  if (value !== lastValue) {
    setLastValue(value);
    if (query(text) !== value) setText(value);
  }
  const [caret, setCaret] = useState(0);
  const [hi, setHi] = useState(-1);
  // A completion the person dismissed stays shut until the text changes.
  const [dismissed, setDismissed] = useState<string>();
  const [help, setHelp] = useState(false);
  const box = useRef<HTMLDivElement>(null);
  const listId = useId();
  const helpId = useId();

  const choices = useChoices(triage);
  const at = valueAt(text, caret);
  const offered = at && dismissed !== text ? matching(choices[at.op], at.typed) : [];
  const open = offered.length > 0;

  const latest = useRef(onSearch);
  useEffect(() => {
    latest.current = onSearch;
  });
  const q = query(text);
  useEffect(() => {
    // A value half typed among offered ones is not a query yet: is:arch
    // would only come back as an error. Enter still searches.
    if (q === value || open) return;
    const t = setTimeout(() => latest.current(q), 300);
    return () => clearTimeout(t);
  }, [q, value, open]);

  // Where the caret goes once React has put an edit's text in the input:
  // before the next keystroke, which a frame later could already be.
  const moveTo = useRef<number>(undefined);
  useLayoutEffect(() => {
    const c = moveTo.current;
    if (c === undefined) return;
    moveTo.current = undefined;
    ref.current?.focus();
    ref.current?.setSelectionRange(c, c);
  });
  const edit = (t: string, c: number) => {
    setText(t);
    setCaret(c);
    setHi(-1);
    setDismissed(undefined);
    moveTo.current = c;
  };
  const accept = (v: ValueAt, c: Choice) => {
    const r = pick(text, v, c.value);
    edit(r.text, r.caret);
  };

  return (
    <div
      ref={box}
      className="relative min-w-0 flex-1 md:max-w-md"
      onBlur={(e) => {
        if (box.current?.contains(e.relatedTarget as Node | null)) return;
        setHelp(false);
        // A scope prefilled and left untouched goes again.
        if (scope && text.trim() === scope && !value) setText("");
      }}
    >
      <SearchIcon
        aria-hidden
        className="pointer-events-none absolute top-1/2 left-2 size-3.5 -translate-y-1/2 text-muted-foreground"
      />
      <Input
        ref={ref}
        // Text rather than search: the browser's own clear button would
        // stand beside ours, and the role is combobox either way.
        type="text"
        enterKeyHint="search"
        role="combobox"
        aria-autocomplete="list"
        aria-expanded={open}
        aria-controls={open ? listId : undefined}
        aria-activedescendant={open && hi >= 0 ? `${listId}-${hi}` : undefined}
        value={text}
        spellCheck={false}
        autoComplete="off"
        onFocus={() => {
          if (text || !scope) return;
          const t = `${scope} `;
          edit(t, t.length);
        }}
        onChange={(e) => {
          setText(e.target.value);
          setCaret(e.target.selectionStart ?? e.target.value.length);
          setHi(-1);
          setDismissed(undefined);
        }}
        onSelect={(e) => setCaret(e.currentTarget.selectionStart ?? 0)}
        onKeyDown={(e) => {
          if (open && at && (e.key === "ArrowDown" || e.key === "ArrowUp")) {
            e.preventDefault();
            const n = offered.length;
            setHi((i) => (e.key === "ArrowDown" ? (i + 1) % n : (i - 1 + n) % n));
          } else if (open && at && (e.key === "Tab" || (e.key === "Enter" && hi >= 0))) {
            e.preventDefault();
            const c = offered[Math.max(hi, 0)];
            if (c) accept(at, c);
          } else if (e.key === "Enter") {
            latest.current(query(text));
          } else if (e.key === "Escape") {
            if (open) setDismissed(text);
            else if (help) setHelp(false);
            else e.currentTarget.blur();
          }
        }}
        placeholder={msg.shell_search_placeholder()}
        aria-label={msg.shell_search_label()}
        className="h-7 pr-13 pl-7"
      />
      <span className="absolute top-1/2 right-1 flex -translate-y-1/2 items-center gap-0.5">
        {text && (
          <button
            type="button"
            aria-label={msg.shell_search_clear()}
            onClick={() => edit("", 0)}
            className="flex size-5 items-center justify-center rounded-sm text-muted-foreground hover:text-foreground"
          >
            <XIcon className="size-3.5" />
          </button>
        )}
        <button
          type="button"
          aria-label={msg.search_help()}
          title={msg.search_help()}
          aria-expanded={help}
          aria-controls={help ? helpId : undefined}
          onClick={() => setHelp((h) => !h)}
          className={cn(
            "flex size-5 items-center justify-center rounded-sm text-muted-foreground hover:text-foreground",
            help && "bg-muted text-foreground",
          )}
        >
          <CircleHelpIcon className="size-3.5" />
        </button>
      </span>
      {open && at && (
        <ul
          id={listId}
          role="listbox"
          aria-label={msg.search_values({ op: `${at.op}:` })}
          className="absolute top-8 left-0 z-30 w-full min-w-56 rounded-md border bg-background p-1 text-foreground shadow-md"
        >
          {offered.map((c, i) => (
            <li
              key={c.value}
              id={`${listId}-${i}`}
              role="option"
              aria-selected={hi === i}
              // Keeps focus in the input, so the arrows and Enter still work.
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => accept(at, c)}
              className={cn(
                "flex h-7 cursor-default items-center gap-2 rounded-sm px-2 text-small hover:bg-muted",
                hi === i && "bg-muted",
              )}
            >
              <span className="min-w-0 truncate font-mono">{c.value}</span>
              {/* Only when the words say more than the value does (phishing: yes). */}
              {c.label && c.label.replace(/\s/g, "") !== c.value && (
                <span className="min-w-0 truncate text-muted-foreground">{c.label}</span>
              )}
              {c.count !== undefined && (
                <span className="ml-auto shrink-0 text-meta text-muted-foreground tabular-nums">
                  {count(c.count)}
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
      {help && (
        <OperatorHelp
          id={helpId}
          bcc={bcc}
          triage={!!triage}
          onPick={(item) => {
            const t = append(text || (scope ?? ""), item);
            setHelp(false);
            edit(t, t.length);
          }}
          onClose={() => {
            setHelp(false);
            ref.current?.focus();
          }}
        />
      )}
    </div>
  );
}

/** The values each completable operator offers, from what is already loaded. */
function useChoices(triage: TriageReport | undefined): Record<Completable, Choice[]> {
  const labels = useLabels().data?.labels ?? [];
  const prefixes = usePrefixes().data?.prefixes ?? [];
  return {
    label: labels.map((l) => ({ value: l.label, count: l.messages })),
    deliveredto: prefixes.map((p) => ({ value: p.address, count: p.messages })),
    class: [
      { value: "machine", label: msg.search_value_machine() },
      { value: "human", label: msg.search_value_human() },
      { value: "unknown", label: msg.search_value_unknown() },
    ],
    is: [
      { value: "archived", label: msg.search_value_archived() },
      { value: "downgraded", label: msg.search_value_downgraded() },
      { value: "sent", label: msg.search_value_sent() },
    ],
    has: [{ value: "attachment", label: msg.search_value_attachment() }],
    triage: byQuestion(triage?.answers ?? []).flatMap((a) =>
      a.options.map((o) => ({
        value: `${a.question}:${o.option}`,
        label: leanLabel({ question: a.question, option: o.option }),
        count: o.messages,
      })),
    ),
  };
}

/**
 * The operators, one example each (§16.3). The examples are query text,
 * so they are not translated; pressing one adds it to the box.
 */
function OperatorHelp({
  id,
  bcc,
  triage,
  onPick,
  onClose,
}: {
  id: string;
  bcc: boolean;
  triage: boolean;
  onPick: (item: string) => void;
  onClose: () => void;
}) {
  const panel = useRef<HTMLDivElement>(null);
  useEffect(() => panel.current?.focus(), []);
  const rows: [string, string][] = [
    ['invoice "order shipped"', msg.search_op_text()],
    ["from:alice@example.com", msg.search_op_from()],
    ["to:bob", msg.search_op_to()],
    ["cc:carol", msg.search_op_cc()],
    ...(bcc ? [["bcc:dave@example.com", msg.search_op_bcc()] as [string, string]] : []),
    ['subject:"weekly report"', msg.search_op_subject()],
    ["body:tracking", msg.search_op_body()],
    ["list:announce", msg.search_op_list()],
    ["deliveredto:shop", msg.search_op_deliveredto()],
    ["label:receipts", msg.search_op_label()],
    ["class:machine", msg.search_op_class()],
    ["is:archived", msg.search_op_is()],
    ["has:attachment", msg.search_op_has()],
    ["after:-7d", msg.search_op_after()],
    ["before:2026-10-01", msg.search_op_before()],
    ...(triage ? [["triage:phishing:yes", msg.search_op_triage()] as [string, string]] : []),
    ["-is:archived", msg.search_op_not()],
    ["(from:alice OR from:bob)", msg.search_op_or()],
  ];
  return (
    <div
      ref={panel}
      id={id}
      role="dialog"
      aria-label={msg.search_help()}
      tabIndex={-1}
      onKeyDown={(e) => {
        if (e.key === "Escape") {
          e.stopPropagation();
          onClose();
        }
      }}
      className="z-30 flex flex-col gap-1.5 rounded-md border bg-background p-2 text-foreground shadow-md outline-none max-md:fixed max-md:inset-x-2 max-md:top-11 max-md:max-h-[calc(100dvh-3.5rem)] max-md:overflow-y-auto md:absolute md:top-8 md:left-0 md:w-[30rem]"
    >
      <p className="text-small text-muted-foreground">
        <ParaglideMessage message={msg.search_help_intro} markup={markup} />
      </p>
      <dl className="grid grid-cols-[auto_1fr] items-baseline gap-x-3 text-small">
        {rows.map(([example, what]) => (
          <div key={example} className="contents">
            <dt>
              <button
                type="button"
                onClick={() => onPick(example)}
                className="rounded-sm px-1 py-0.5 text-left font-mono whitespace-nowrap hover:bg-muted focus-visible:bg-muted focus-visible:outline-none"
              >
                {example}
              </button>
            </dt>
            <dd className="min-w-0 text-muted-foreground">{what}</dd>
          </div>
        ))}
      </dl>
    </div>
  );
}
