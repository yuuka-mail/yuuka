import { useQueryClient } from "@tanstack/react-query";
import { ArrowLeftIcon, ExternalLinkIcon } from "lucide-react";
import { useState } from "react";

import { $api, api, data } from "@/api/client";
import { invalidateAnnotated, useProposals } from "@/api/queries";
import type { AcceptResult, Cite, Proposal } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Signature, system, Voice } from "@/components/voice";
import { listTime, sender } from "@/lib/format";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";
import { RuleDrafter } from "@/rules/drafter";
import { asMatch, ruleActionsOf } from "@/rules/match";
import { RuleEditor } from "@/rules/rule-editor";
import { ActionsWords, MatchWords } from "@/rules/words";
import { ProblemLine } from "@/send/marks";
import { problem, type Abilities } from "@/send/outgoing";

import { Outcomes } from "./annotate";
import { useMessage } from "./reading-data";

// Proposals (§8.5) are an agent speaking: its guess, its reasons, the
// passages it relied on. They grow beside the mail they are about
// (§12.5, §12.12) and only a person decides them. The rationale and the
// quotes are the agent's text and the mail's: shown as text, never as
// markup, and a cite opens the passage so it can be checked.

const citePattern = /^yuuka:\/\/message\/([0-9A-Za-z]+)(?:#(.+))?$/;

const agentOf = (p: Proposal) => (p.proposer.kind === "agent" ? p.proposer.name : `${p.proposer.kind}:${p.proposer.name}`);

/** The pending proposals about one message, in the reading pane. */
export function MessageProposals({ id, highlight, can, onOpen }: { id: string; highlight?: string; can: Abilities; onOpen: (id: string, anchor?: string) => void }) {
  const q = useProposals({ message_id: id });
  // Decided ones are history, not decisions; expired ones are shown
  // greyed until they would no longer be news.
  const ps = (q.data?.proposals ?? []).filter((p) => p.state === "pending" || p.state === "expired" || p.id === highlight);
  if (!ps.length) return null;
  return (
    <div className="flex flex-col gap-2">
      {ps.map((p) => (
        <ProposalCard key={p.id} p={p} can={can} onOpen={onOpen} highlight={p.id === highlight} />
      ))}
    </div>
  );
}

type Deciding = { s: "idle" } | { s: "automate" } | { s: "review" } | { s: "confirm-reject" };

export function ProposalCard({ p, can, onOpen, highlight }: { p: Proposal; can: Abilities; onOpen: (id: string, anchor?: string) => void; highlight?: boolean }) {
  const qc = useQueryClient();
  const [step, setStep] = useState<Deciding>({ s: "idle" });
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const [accepted, setAccepted] = useState<AcceptResult>();
  const match = asMatch(p.match);
  const pending = p.state === "pending";
  const expired = p.state === "expired";
  const decide = async (f: () => Promise<unknown>) => {
    setBusy(true);
    setErr(undefined);
    try {
      await f();
      invalidateAnnotated(qc);
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };
  const accept = () =>
    decide(async () => {
      setAccepted(await data(api.POST("/api/v1/proposals/{id}/accept", { params: { path: { id: p.id } }, body: {} })));
    });
  const reject = () => decide(() => data(api.POST("/api/v1/proposals/{id}/reject", { params: { path: { id: p.id } } })));
  const what = p.kind === "rule" ? m.proposal_kind_rule() : p.scope === "thread" ? m.proposal_kind_thread() : m.proposal_kind_message();

  return (
    <Voice
      speaker={{ kind: "agent", name: agentOf(p) }}
      title={`${what} · ${listTime(p.created_at)}`}
      className={cn(expired && "opacity-60", highlight && "ring-1 ring-ring/40")}
      action={<StateMark p={p} />}
    >
      <div className="flex flex-col gap-1.5">
        <div className="flex flex-col gap-0.5">
          {match && <MatchWords match={match} />}
          <ActionsWords actions={p.actions} />
        </div>
        {p.rationale && <p className="whitespace-pre-wrap break-words">{p.rationale}</p>}
        {p.cites.length > 0 && (
          <ul className="flex flex-col gap-0.5">
            {p.cites.map((c, i) => (
              <CiteLine key={i} c={c} onOpen={onOpen} />
            ))}
          </ul>
        )}
        {accepted ? (
          accepted.messages.length > 0 ? (
            <Outcomes done={{ result: { action_id: accepted.proposal.action_id, messages: accepted.messages } }} />
          ) : (
            <p className="text-system-foreground">{m.proposal_accepted()}</p>
          )
        ) : expired ? (
          <p className="flex items-center gap-2">
            <Signature speaker={system} />
            {m.proposal_expired()}
          </p>
        ) : (
          pending &&
          can.decide && (
            <div className="flex flex-wrap items-center gap-1.5">
              {p.kind === "annotate" ? (
                <>
                  <Button size="xs" disabled={busy} onClick={() => void accept()}>
                    {m.proposal_accept()}
                  </Button>
                  {can.rules && p.message_id && (
                    <Button size="xs" variant="outline" aria-expanded={step.s === "automate"} onClick={() => setStep(step.s === "automate" ? { s: "idle" } : { s: "automate" })}>
                      {m.proposal_accept_automate()}
                    </Button>
                  )}
                </>
              ) : (
                can.rules && (
                  <Button size="xs" aria-expanded={step.s === "review"} onClick={() => setStep(step.s === "review" ? { s: "idle" } : { s: "review" })}>
                    {m.proposal_review_rule()}
                  </Button>
                )
              )}
              {step.s === "confirm-reject" ? (
                <>
                  <Button size="xs" variant="destructive" disabled={busy} onClick={() => void reject()}>
                    {m.proposal_reject_confirm()}
                  </Button>
                  <Button size="xs" variant="ghost" onClick={() => setStep({ s: "idle" })}>
                    {m.rule_cancel()}
                  </Button>
                </>
              ) : (
                <Button size="xs" variant="ghost" onClick={() => setStep({ s: "confirm-reject" })}>
                  {m.proposal_reject()}
                </Button>
              )}
              {p.kind === "rule" && !can.rules && <span className="text-muted-foreground">{m.proposal_needs_rules()}</span>}
            </div>
          )
        )}
        {err !== undefined && <ProblemLine p={problem(err, { conflict: m.proposal_conflict })} />}
        {step.s === "automate" && p.message_id && (
          <RuleDrafter messageId={p.message_id} initialActions={ruleActionsOf(p.actions)} proposal={p.id} onClose={() => setStep({ s: "idle" })} onOpen={(id) => onOpen(id)} />
        )}
        {step.s === "review" && (
          <div className="rounded-md border bg-background px-3 py-2">
            <RuleEditor
              initial={{ name: "", priority: 0, enabled: true, match: p.match, actions: p.actions }}
              showEnabled={false}
              submitLabel={m.proposal_accept_rule()}
              onOpen={(id) => onOpen(id)}
              onSubmit={async (rule) => {
                const r = await data(api.POST("/api/v1/proposals/{id}/accept", { params: { path: { id: p.id } }, body: { rule } }));
                invalidateAnnotated(qc);
                setAccepted(r);
                setStep({ s: "idle" });
                return r.rule?.warnings;
              }}
            />
          </div>
        )}
      </div>
    </Voice>
  );
}

function StateMark({ p }: { p: Proposal }) {
  if (p.state === "pending") return <span className="text-meta text-muted-foreground">{m.proposal_expires({ when: listTime(p.expires_at) })}</span>;
  const words: Record<Proposal["state"], () => string> = {
    pending: m.proposal_state_pending,
    accepted: m.proposal_state_accepted,
    rejected: m.proposal_state_rejected,
    withdrawn: m.proposal_state_withdrawn,
    expired: m.proposal_state_expired,
  };
  return <span className="text-meta text-muted-foreground">{words[p.state]()}</span>;
}

/** A passage the agent relied on: its quote as text, and the anchor to check it at. */
export function CiteLine({ c, onOpen }: { c: Cite; onOpen: (id: string, anchor?: string) => void }) {
  const hit = citePattern.exec(c.uri);
  return (
    <li className="flex min-w-0 items-baseline gap-1.5">
      <span className="shrink-0 text-muted-foreground">{m.proposal_cites()}</span>
      {c.quote && (
        <span lang="" className="min-w-0 truncate italic">
          “{c.quote}”
        </span>
      )}
      {hit ? (
        <button type="button" className="inline-flex shrink-0 items-center gap-0.5 font-mono text-meta underline underline-offset-2" onClick={() => onOpen(hit[1] ?? "", hit[2])}>
          {hit[2] ?? m.proposal_cite_message()}
          <ExternalLinkIcon aria-hidden className="size-3" />
        </button>
      ) : (
        <span className="font-mono text-meta break-all">{c.uri}</span>
      )}
    </li>
  );
}

/**
 * The list of proposals waiting for a person (the sidebar's attention
 * entry, §12.9): pending ones newest first, then the expired, greyed.
 * Picking one opens the mail it is about, where it is decided.
 */
export function ProposalList({ selected, onPick }: { selected?: string; onPick: (p: Proposal) => void }) {
  const pending = useProposals({ state: "pending" });
  const expired = useProposals({ state: "expired" });
  if (pending.error) {
    return (
      <p className="p-4">
        <ProblemLine p={problem(pending.error)} />
      </p>
    );
  }
  if (!pending.data) {
    return (
      <div className="flex flex-col gap-3 p-3">
        <Skeleton className="h-3.5 w-2/3" />
        <Skeleton className="h-3.5 w-1/2" />
      </div>
    );
  }
  const ps = [...pending.data.proposals, ...(expired.data?.proposals ?? [])];
  if (!ps.length) return <p className="p-4 text-muted-foreground">{m.proposal_none()}</p>;
  return (
    <ul aria-label={m.area_proposals()} className="min-h-0 flex-1 overflow-y-auto">
      {ps.map((p) => (
        <li key={p.id} className="h-14">
          <ProposalRow p={p} selected={p.id === selected} onPick={onPick} />
        </li>
      ))}
    </ul>
  );
}

function ProposalRow({ p, selected, onPick }: { p: Proposal; selected: boolean; onPick: (p: Proposal) => void }) {
  const about = useMessage(p.message_id ?? "", !!p.message_id);
  const subj = about.data && (about.data.subject || sender(about.data.from_name, about.data.from_addr));
  return (
    <button
      type="button"
      aria-current={selected || undefined}
      onClick={() => onPick(p)}
      className={cn(
        "flex h-full w-full flex-col justify-center gap-0.5 border-b px-3 text-left outline-none hover:bg-muted focus-visible:bg-muted",
        selected && "bg-accent text-accent-foreground shadow-[inset_2px_0_0_var(--color-foreground)] hover:bg-accent",
        p.state === "expired" && "text-muted-foreground opacity-60",
      )}
    >
      <span className="flex min-w-0 items-center gap-1.5 text-ui">
        <span className="inline-flex shrink-0 items-center gap-1 text-meta font-semibold text-agent-foreground">
          <span aria-hidden className="size-1.5 rounded-full bg-agent" />
          {agentOf(p)}
        </span>
        <span className="min-w-0 truncate">
          <ActionsWords actions={p.actions} />
        </span>
        <span className="ml-auto shrink-0 pl-2 text-meta text-muted-foreground tabular-nums">
          {p.state === "expired" ? m.proposal_state_expired() : listTime(p.created_at)}
        </span>
      </span>
      <span lang="" className="truncate text-small text-muted-foreground">
        {p.kind === "rule" ? `${m.proposal_kind_rule()} · ` : ""}
        {subj ?? p.rationale}
      </span>
    </button>
  );
}

/** The pending count for the sidebar; more than a page reads as "100+". */
export function usePendingProposals(enabled: boolean): { n?: number; more: boolean } {
  const q = useProposals({ state: "pending" }, enabled);
  return { n: q.data?.proposals.length, more: !!q.data?.next_cursor };
}

/** A proposal about no message, alone in the main column. */
export function StandaloneProposal({ id, can, onBack, onOpen }: { id: string; can: Abilities; onBack: () => void; onOpen: (id: string, anchor?: string) => void }) {
  const q = $api.useQuery("get", "/api/v1/proposals/{id}", { params: { path: { id } } });
  return (
    <article className="flex h-full min-h-0 flex-col">
      <div className="flex h-11 shrink-0 items-center border-b px-2 md:hidden">
        <Button variant="ghost" size="sm" onClick={onBack}>
          <ArrowLeftIcon data-icon="inline-start" />
          {m.reading_back()}
        </Button>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-4 md:px-6">
        {q.data ? <ProposalCard p={q.data} can={can} onOpen={onOpen} /> : q.error ? <ProblemLine p={problem(q.error)} /> : <Skeleton className="h-24 w-full" />}
      </div>
    </article>
  );
}
