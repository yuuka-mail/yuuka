import { useState } from "react";

import { useTriageReport } from "@/api/queries";
import type { TriageReport } from "@/api/types";
import { HostCommand } from "@/components/cli";
import { Skeleton } from "@/components/ui/skeleton";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { Signature } from "@/components/voice";
import { count } from "@/lib/format";
import { m } from "@/paraglide/messages.js";
import { byQuestion, leanLabel, leanParam, optionLabel } from "@/mail/triage";
import { QuestionName } from "@/mail/triage-box";
import { ProblemLine } from "@/send/marks";
import { problem } from "@/send/outgoing";

import { SectionHeader } from "./section";

// The scorer's report (§15.5): how the latest estimates fall over the mail
// the viewer sees, the counts `yuuka triage report` prints. People only.
// It is the shadow list in numbers: what the model would flag if it were
// allowed to act, which it is not (§15.4). Each count opens that mail.

const windows = { all: undefined, d30: 30, d7: 7 } as const;
type Window = keyof typeof windows;

const sinceOf = (w: Window) => {
  const days = windows[w];
  return days === undefined
    ? undefined
    : new Date(Date.now() - days * 24 * 3600 * 1000).toISOString();
};

export function TriageReportSection({ onLean }: { onLean: (tr: string) => void }) {
  // The since time is fixed when the window is picked, so the query key holds still.
  const [w, setW] = useState<{ window: Window; since?: string }>({ window: "all" });
  const q = useTriageReport(w.since);
  return (
    <div className="flex max-w-[64rem] flex-col gap-3 px-4 py-4 md:px-6">
      <SectionHeader title={m.settings_triage()}>
        <ToggleGroup
          size="xs"
          variant="outline"
          spacing={0}
          aria-label={m.triage_report_window()}
          value={[w.window]}
          onValueChange={(v: string[]) => {
            const next = v[0] as Window | undefined;
            if (next) setW({ window: next, since: sinceOf(next) });
          }}
        >
          <ToggleGroupItem value="all">{m.triage_report_all()}</ToggleGroupItem>
          <ToggleGroupItem value="d30">{m.triage_report_days({ count: 30 })}</ToggleGroupItem>
          <ToggleGroupItem value="d7">{m.triage_report_days({ count: 7 })}</ToggleGroupItem>
        </ToggleGroup>
      </SectionHeader>
      <p className="text-small text-muted-foreground">{m.triage_report_hint()}</p>
      {q.scorer === false ? (
        <HostCommand why={m.triage_report_none()} command="yuuka triage status" />
      ) : q.error ? (
        <ProblemLine p={problem(q.error)} />
      ) : !q.report ? (
        <Skeleton className="h-40 w-full" />
      ) : (
        <Report r={q.report} onLean={onLean} />
      )}
    </div>
  );
}

function Report({ r, onLean }: { r: TriageReport; onLean: (tr: string) => void }) {
  return (
    <section
      aria-label={m.settings_triage()}
      className="flex flex-col gap-2 rounded-md border border-observation-border bg-observation-surface px-3 py-2 text-small"
    >
      <header className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
        {/* The report names the scorer; each score names its model. */}
        <Signature speaker={{ kind: "observation", scorer: r.scorer, model: "" }} />
        <span className="text-muted-foreground">
          {m.triage_report_totals({
            scored: m.triage_report_scored({ count: r.scored }),
            unscored: m.triage_report_unscored({ count: r.unscored }),
            version: r.questions,
          })}
        </span>
      </header>
      <dl className="grid grid-cols-1 gap-x-4 gap-y-2 sm:grid-cols-[auto_1fr]">
        {byQuestion(r.answers).map((a) => (
          <div key={a.question} className="contents">
            <dt className="text-muted-foreground">
              <QuestionName q={a.question} />
            </dt>
            <dd className="flex min-w-0 flex-col gap-0.5">
              {a.options.map((o) => (
                <button
                  key={o.option}
                  type="button"
                  title={m.triage_report_open_lean({
                    lean: leanLabel({ question: a.question, option: o.option }),
                  })}
                  onClick={() => onLean(leanParam({ question: a.question, option: o.option }))}
                  className="-mx-1 flex items-center gap-2 rounded-sm px-1 text-left hover:bg-background/60"
                >
                  <span className="w-28 shrink-0 truncate underline-offset-2 hover:underline">
                    {optionLabel(o.option)}
                  </span>
                  <span
                    aria-hidden
                    className="h-1.5 w-32 shrink-0 overflow-hidden rounded-full bg-observation-border"
                  >
                    <span
                      className="block h-full rounded-full bg-observation"
                      style={{ width: `${r.scored ? (o.messages / r.scored) * 100 : 0}%` }}
                    />
                  </span>
                  <span className="text-observation-foreground tabular-nums">
                    {count(o.messages)}
                  </span>
                </button>
              ))}
              {a.abstained > 0 && (
                <span className="text-muted-foreground">
                  {m.triage_report_abstained({ count: a.abstained })}
                </span>
              )}
            </dd>
          </div>
        ))}
      </dl>
      {/* The server's own caveat, as it wrote it (§12.11): a note, not translated. */}
      <p lang="en" className="text-meta text-muted-foreground">
        {r.caveat}
      </p>
    </section>
  );
}
