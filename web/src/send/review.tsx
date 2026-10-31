import { useQueryClient } from "@tanstack/react-query";
import { CheckIcon, TriangleAlertIcon } from "lucide-react";
import { useState } from "react";

import { api, data } from "@/api/client";
import type { Review, ReviewCheck, Withdrawal } from "@/api/types";
import { HostCommand } from "@/components/cli";
import { Button } from "@/components/ui/button";
import { fullTime, listTime } from "@/lib/format";
import { useNow } from "@/lib/now";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";

import { ProblemLine } from "./marks";
import { checkLabel, problem, resumeCommand } from "./outgoing";

// The review tier as yuuka reports it (§8.9): what each check found, how
// long held mail waits, and recalling it. All of it is the system
// speaking. The verdict says where a code or sign-in link was found and
// which mail it came from, never the value: the value is the secret, and
// the server does not keep it in the verdict either. Everything quoted
// from the outgoing mail (a cited URI, a host) is shown as text.

/**
 * The checks of a review. Gates that failed escalated it and list what
 * tripped them; advisory checks that failed (cross_thread, link_domains)
 * held nothing back and are reported for the recall window, under their
 * own heading so they never read as the reason it stopped. Passed ones
 * fold into one line. With only="failed" the passed line is left out.
 */
export function ReviewVerdict({
  review,
  onOpenMessage,
  only,
}: {
  review: Review;
  onOpenMessage?: (id: string) => void;
  only?: "failed";
}) {
  const failed = review.checks.filter((c) => !c.pass && !c.advisory);
  const noted = review.checks.filter((c) => !c.pass && c.advisory);
  const passed = review.checks.filter((c) => c.pass);
  const list = (cs: ReviewCheck[], label: string) => (
    <ul aria-label={label} className="flex flex-col gap-1">
      {cs.map((c) => (
        <li key={c.name} className="flex flex-col gap-0.5">
          <span className="flex items-center gap-1.5 font-medium text-warning">
            <TriangleAlertIcon aria-hidden className="size-3 shrink-0" />
            {checkLabel(c.name)}
          </span>
          <Findings c={c} onOpenMessage={onOpenMessage} />
        </li>
      ))}
    </ul>
  );
  return (
    <div className="flex flex-col gap-1">
      {failed.length > 0 && list(failed, m.review_escalated_label())}
      {noted.length > 0 && (
        <div className="flex flex-col gap-0.5">
          <p className="text-muted-foreground">{m.review_advisory()}</p>
          {list(noted, m.review_advisory())}
        </div>
      )}
      {passed.length > 0 && only !== "failed" && (
        <p className="flex flex-wrap items-center gap-x-1.5 text-muted-foreground">
          <CheckIcon aria-hidden className="size-3 shrink-0 text-system-foreground" />
          <span>
            {m.review_passed({ checks: passed.map((c) => checkLabel(c.name)).join(" · ") })}
          </span>
        </p>
      )}
    </div>
  );
}

/** What one failed check found, in the one list that fits it. */
function Findings({ c, onOpenMessage }: { c: ReviewCheck; onOpenMessage?: (id: string) => void }) {
  const open = (id: string) =>
    onOpenMessage ? (
      <button
        type="button"
        className="font-mono underline underline-offset-2 hover:text-foreground"
        onClick={() => onOpenMessage(id)}
      >
        {id}
      </button>
    ) : (
      <span className="font-mono">{id}</span>
    );
  const items: React.ReactNode[] = [];
  for (const r of c.recipients ?? []) {
    items.push(
      <>
        <span className="font-mono">{r}</span> {m.review_never_written()}
      </>,
    );
  }
  for (const x of c.extracted ?? []) {
    items.push(
      <>
        {x.kind === "link"
          ? m.review_found_link({ where: x.where })
          : m.review_found_code({ where: x.where })}{" "}
        {open(x.message_id)} <span className="text-muted-foreground">({x.source})</span>
      </>,
    );
  }
  for (const r of c.refs ?? []) {
    items.push(
      <>
        <span className="font-mono wrap-anywhere">{r.as}</span>{" "}
        <span className="text-muted-foreground">({r.where})</span>{" "}
        {r.message_id ? (
          <>
            {m.review_ref_other_thread()} {open(r.message_id)}
          </>
        ) : (
          m.review_ref_unknown()
        )}
      </>,
    );
  }
  for (const l of c.links ?? []) {
    items.push(
      <>
        <span className="font-mono">{l.host}</span>{" "}
        <span className="text-muted-foreground">({l.where})</span>{" "}
        {l.domain ? m.review_link_unseen({ domain: l.domain }) : m.review_link_not_domain()}
      </>,
    );
  }
  for (const u of c.unreadable ?? []) {
    items.push(
      <>
        <span className="font-mono">{u}</span> {m.review_unreadable()}
      </>,
    );
  }
  // A check that names nothing (too many links, say) says why in its detail.
  if (items.length === 0) return <p className="pl-4.5 text-muted-foreground">{c.detail}</p>;
  return (
    <ul className="flex flex-col pl-4.5">
      {items.map((x, i) => (
        <li key={i} className="wrap-anywhere">
          {x}
        </li>
      ))}
    </ul>
  );
}

/**
 * How long until held mail goes, in the largest whole unit. Held mail is
 * sent when its window ends; it does not lapse, so these words are never
 * the "left"/"expires" of a code's countdown (mail/extracted.ts).
 */
export function untilSent(
  until: string,
  now: number,
): { unit: "s" | "min" | "h"; n: number } | undefined {
  const s = Math.floor((Date.parse(until) - now) / 1000);
  if (Number.isNaN(s) || s <= 0) return undefined;
  if (s < 60) return { unit: "s", n: s };
  const min = Math.ceil(s / 60);
  return min < 120 ? { unit: "min", n: min } : { unit: "h", n: Math.floor(min / 60) };
}

const sendsIn = { s: m.review_sends_in_s, min: m.review_sends_in_min, h: m.review_sends_in_h };
const recallLeft = {
  s: m.review_recall_left_s,
  min: m.review_recall_left_min,
  h: m.review_recall_left_h,
};

/** "Sends at 04:42 unless recalled · 58 min to recall", refreshed as it counts down. */
export function HoldUntil({ until }: { until: string }) {
  const now = useNow(15_000);
  const left = untilSent(until, now);
  return (
    <span title={fullTime(until)}>
      <span className="font-medium">
        {left ? m.review_sends_at({ time: listTime(until) }) : m.review_sends_now()}
      </span>
      {left && (
        <span className="text-system-foreground tabular-nums">
          {" "}
          · {recallLeft[left.unit]({ n: left.n })}
        </span>
      )}
    </span>
  );
}

/**
 * Recall held mail back to its draft (§8.9): pick it, then press again,
 * with the cost beside the second press. A person's recall counts toward
 * the grant's suspension; when it trips, yuuka says so and names the host
 * command that resumes the grant.
 */
export function Recall({
  id,
  identity,
  submitter,
  onRecalled,
  onOpenDraft,
}: {
  id: string;
  /** The identity's address and the submitter (kind:name): whose grant a recall counts against. */
  identity: string;
  submitter: string;
  onRecalled?: (w: Withdrawal) => void;
  onOpenDraft?: (id: string) => void;
}) {
  const qc = useQueryClient();
  const [confirm, setConfirm] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const [done, setDone] = useState<Withdrawal>();
  const recall = async () => {
    setBusy(true);
    setErr(undefined);
    try {
      const w = await data(api.POST("/api/v1/outbox/{id}/withdraw", { params: { path: { id } } }));
      setDone(w);
      onRecalled?.(w);
    } catch (x) {
      setErr(x);
    } finally {
      setBusy(false);
      setConfirm(false);
      for (const k of [
        "/api/v1/outbox",
        "/api/v1/outbox/{id}",
        "/api/v1/drafts",
        "/api/v1/drafts/{id}",
        "/api/v1/messages",
        "/api/v1/threads/{id}",
      ]) {
        void qc.invalidateQueries({ queryKey: ["get", k] });
      }
    }
  };
  if (done)
    return (
      <Recalled w={done} identity={identity} submitter={submitter} onOpenDraft={onOpenDraft} />
    );
  return (
    <div className="flex flex-col gap-1">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        {confirm ? (
          <>
            <Button size="xs" variant="destructive" disabled={busy} onClick={() => void recall()}>
              {m.review_recall_confirm()}
            </Button>
            <Button size="xs" variant="ghost" onClick={() => setConfirm(false)}>
              {m.rule_cancel()}
            </Button>
            <span className="text-muted-foreground">{m.review_recall_cost()}</span>
          </>
        ) : (
          <Button size="xs" variant="outline" onClick={() => setConfirm(true)}>
            {m.review_recall()}
          </Button>
        )}
      </div>
      {err !== undefined && (
        <ProblemLine p={problem(err, { conflict: m.review_recall_conflict })} />
      )}
    </div>
  );
}

/** What a recall did: back to the draft, and the grant suspended if it tripped. */
export function Recalled({
  w,
  identity,
  submitter,
  onOpenDraft,
}: {
  w: Withdrawal;
  identity: string;
  submitter: string;
  onOpenDraft?: (id: string) => void;
}) {
  const s = w.suspended;
  return (
    <div className="flex flex-col gap-1.5">
      <p className="flex flex-wrap items-baseline gap-x-2">
        <span className="font-medium text-system-foreground">{m.review_recalled()}</span>
        {onOpenDraft && (
          <button
            type="button"
            className="underline underline-offset-2"
            onClick={() => onOpenDraft(w.draft_id)}
          >
            {m.send_open_draft()}
          </button>
        )}
      </p>
      {s && (
        <HostCommand
          className="border-warning/60"
          why={
            <>
              <span className="font-medium text-warning">
                {m.review_suspended({ who: submitter, identity })}
              </span>{" "}
              {s.withdrawn.length > 0 && m.review_suspended_others({ n: s.withdrawn.length })}{" "}
              {m.review_resume_on_host()}
            </>
          }
          command={resumeCommand(identity, submitter)}
        />
      )}
    </div>
  );
}

/** When a held row goes, in a list: "sends in 58 min". */
export function HeldLeft({ until, className }: { until: string; className?: string }) {
  const now = useNow(15_000);
  const left = untilSent(until, now);
  return (
    <span className={cn("shrink-0 text-meta text-system-foreground tabular-nums", className)}>
      {left ? sendsIn[left.unit]({ n: left.n }) : m.review_sends_now()}
    </span>
  );
}
