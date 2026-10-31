import { ChevronDownIcon, ChevronUpIcon } from "lucide-react";
import { useState, type ReactNode } from "react";

import type { TriageAnswer, TriageInput, TriageReport, TriageScore } from "@/api/types";
import { Mark, Signature, type Speaker } from "@/components/voice";
import { fullTime, listTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";

import {
  byQuestion,
  leanChoices,
  leanLabel,
  leanParam,
  markLabel,
  notableLeanings,
  optionLabel,
  parseLean,
  pct,
  questionLabel,
  ranked,
  type Lean,
} from "./triage";

// A scorer's estimates (§15) in the observation voice (§12.5): its own
// colour and diamond mark, signed with the scorer and the model, never
// with a principal. Every place that shows one says it is a model's
// estimate and that nothing moved because of it (§15.4): the score is
// shown, and it acts on nothing.

const speaker = (s: TriageScore): Speaker => ({
  kind: "observation",
  scorer: s.scorer,
  model: s.model,
});

/**
 * The scorer's estimate of the open message: one compact box with the
 * option each question leans to, opened to every probability and, for a
 * person who may see the delivery facts, what the scorer was given.
 * Nothing at all without a score.
 */
export function TriageBox({ score }: { score: TriageScore | undefined }) {
  const [open, setOpen] = useState(false);
  if (!score) return null;
  const answers = byQuestion(score.answers);
  return (
    <section
      aria-label={m.triage_label()}
      className={cn(
        "flex max-w-full min-w-0 flex-col gap-1 rounded-md border border-observation-border bg-observation-surface px-2 py-1 text-small",
        open ? "px-3 py-2" : "self-start",
      )}
    >
      <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5">
        <Signature speaker={speaker(score)} />
        {open ? (
          <span className="text-muted-foreground">
            {m.triage_scored({ when: listTime(score.scored_at) })}
          </span>
        ) : (
          answers.map((a) => <TopAnswer key={a.question} a={a} />)
        )}
        <button
          type="button"
          aria-expanded={open}
          aria-label={open ? m.triage_fold() : m.triage_unfold()}
          title={open ? m.triage_fold() : m.triage_unfold()}
          onClick={() => setOpen((o) => !o)}
          className={cn(
            "flex shrink-0 text-muted-foreground hover:text-foreground",
            open && "ml-auto",
          )}
        >
          {open ? (
            <ChevronUpIcon aria-hidden className="size-3" />
          ) : (
            <ChevronDownIcon aria-hidden className="size-3" />
          )}
        </button>
      </div>
      {open && (
        <>
          <dl className="mt-1 grid grid-cols-1 gap-x-3 gap-y-1.5 sm:grid-cols-[auto_1fr]">
            {answers.map((a) => (
              <AllProbabilities key={a.question} a={a} />
            ))}
          </dl>
          <p className="text-meta text-muted-foreground">
            {m.triage_provenance({
              when: fullTime(score.scored_at),
              version: score.questions,
            })}
          </p>
          {score.input && <Input input={score.input} />}
        </>
      )}
      <p className="text-meta text-observation-foreground">{m.triage_notice()}</p>
    </section>
  );
}

/** One answer in the folded line: the option it leans to and how much, or that it abstained. */
function TopAnswer({ a }: { a: TriageAnswer }) {
  const top = ranked(a)[0];
  return (
    <span className="whitespace-nowrap">
      <span className="text-muted-foreground">{questionLabel(a.question)} </span>
      {a.abstained || !top ? (
        <span className="text-muted-foreground italic">{m.triage_abstained()}</span>
      ) : (
        <>
          <span className="font-medium">{optionLabel(top.option)}</span>{" "}
          <span className="text-observation-foreground tabular-nums">{pct(top.p)}</span>
        </>
      )}
    </span>
  );
}

/**
 * A question by its words, with its key beside them when they differ: the
 * key is what the filter and agents name it.
 */
export function QuestionName({ q }: { q: string }) {
  const label = questionLabel(q);
  return (
    <>
      {label}
      {label !== q && (
        <>
          {" "}
          <span className="font-mono text-meta" title={m.triage_question_key()}>
            {q}
          </span>
        </>
      )}
    </>
  );
}

/** Every option of one answer with its probability, as a bar. */
function AllProbabilities({ a }: { a: TriageAnswer }) {
  const opts = ranked(a);
  return (
    <div className="contents">
      <dt className="text-muted-foreground">
        <QuestionName q={a.question} />
      </dt>
      <dd className="flex min-w-0 flex-col gap-0.5">
        {a.abstained || opts.length === 0 ? (
          <span className="text-muted-foreground">{m.triage_abstained_detail()}</span>
        ) : (
          opts.map((o, i) => (
            <span key={o.option} className="flex items-center gap-2">
              <span className={cn("w-20 shrink-0 truncate sm:w-24", i === 0 && "font-medium")}>
                {optionLabel(o.option)}
              </span>
              <span
                aria-hidden
                className="h-1.5 max-w-24 min-w-8 flex-1 overflow-hidden rounded-full bg-observation-border"
              >
                <span
                  className="block h-full rounded-full bg-observation"
                  style={{ width: `${Math.round(o.p * 100)}%` }}
                />
              </span>
              <span className="shrink-0 text-observation-foreground tabular-nums">{pct(o.p)}</span>
            </span>
          ))
        )}
      </dd>
    </div>
  );
}

const yn = (b: boolean) => (b ? m.triage_o_yes() : m.triage_o_no());

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="contents">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0 wrap-anywhere">{children}</dd>
    </div>
  );
}

/**
 * What the scorer was given (§15.2): the facts yuuka passed along, as they
 * were then, and how much text; never the text. Only a person who may see
 * the delivery facts gets these, which the server decides.
 */
function Input({ input: i }: { input: TriageInput }) {
  const f = i.facts;
  return (
    <section
      aria-label={m.triage_input_label()}
      className="mt-1 border-t border-observation-border pt-1.5"
    >
      <h3 className="mb-0.5 font-medium">{m.triage_input_label()}</h3>
      <p className="mb-1 text-meta text-muted-foreground">{m.triage_input_hint()}</p>
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5">
        <Fact label={m.triage_in_endpoint()}>
          {i.endpoint === "hosted" ? (
            <span className="font-medium text-warning">{m.triage_in_hosted()}</span>
          ) : (
            m.triage_in_local()
          )}
        </Fact>
        <Fact label={m.triage_in_auth()}>
          <span className="font-mono">{f.auth || "—"}</span>
        </Fact>
        <Fact label={m.triage_in_claimed()}>{yn(f.claimed)}</Fact>
        <Fact label={m.triage_in_known_sender()}>{yn(f.known_sender)}</Fact>
        <Fact label={m.triage_in_reply_to_other()}>{yn(f.reply_to_other)}</Fact>
        <Fact label={m.triage_in_list_id()}>{yn(f.list_id)}</Fact>
        <Fact label={m.triage_in_class()}>
          <span className="font-mono">{f.class}</span>
        </Fact>
        <Fact label={m.triage_in_code()}>{yn(f.has_code)}</Fact>
        <Fact label={m.triage_in_link()}>{yn(f.has_link)}</Fact>
        <Fact label={m.triage_in_text()}>
          {m.triage_in_chars({ count: i.chars })}
          {i.truncated && (
            <span className="text-muted-foreground"> · {m.triage_in_truncated()}</span>
          )}
        </Fact>
        <Fact label={m.triage_in_versions()}>
          <span className="font-mono">
            {m.triage_in_view_parser({ view: i.view, parser: i.parser })}
          </span>
        </Fact>
        <Fact label="sha256">
          <span className="font-mono break-all" title={i.sha256}>
            {i.sha256.slice(0, 16)}…
          </span>
        </Fact>
      </dl>
    </section>
  );
}

/**
 * A row's quiet mark for the leanings worth a glance (§15.4): the
 * diamond and a word, in the observation voice, never the warning
 * colour. The mail is where it always was; the mark only says what a
 * model guessed.
 */
export function LeanMarks({ tops }: { tops: Record<string, string> | undefined }) {
  const ls = notableLeanings(tops);
  if (!ls.length) return null;
  return (
    <>
      {ls.map((l) => (
        <span
          key={leanParam(l)}
          title={m.triage_mark_title({ lean: leanLabel(l) })}
          className="inline-flex shrink-0 items-center gap-1 text-meta text-observation-foreground"
        >
          <Mark kind="observation" />
          {markLabel(l)}
          <span className="sr-only">{m.triage_mark_title({ lean: leanLabel(l) })}</span>
        </span>
      ))}
    </>
  );
}

/**
 * The filter by leaning (§15.4): the options the report counted, each
 * with its count. One is always picked; a search narrows by leaning with
 * triage: in its text (§16.5), which the box completes from the same report.
 */
export function LeanFilter({
  report,
  value,
  onChange,
}: {
  report: TriageReport | undefined;
  value: string | undefined;
  onChange: (tr: string | undefined) => void;
}) {
  const current = parseLean(value);
  const choices = leanChoices(report, current);
  return (
    <label className="flex min-w-0 items-center gap-1.5 text-small text-observation-foreground">
      <Mark kind="observation" />
      <span className="shrink-0">{m.triage_filter_label()}</span>
      <select
        className="h-6 min-w-0 rounded-md border border-observation-border bg-background px-1 text-small text-foreground"
        value={value ?? ""}
        onChange={(e) => onChange(e.target.value || undefined)}
      >
        {choices.map((l: Lean & { messages?: number }) => (
          <option key={leanParam(l)} value={leanParam(l)}>
            {l.messages === undefined
              ? leanLabel(l)
              : m.triage_filter_option({ lean: leanLabel(l), count: l.messages })}
          </option>
        ))}
      </select>
    </label>
  );
}
