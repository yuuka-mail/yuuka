import type { OutboxEntry } from "@/api/types";
import { fullTime, listTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";

import { toneText } from "./marks";
import { outboxState, principal, recipientState } from "./outgoing";

/**
 * What yuuka knows about one outbox row: its state, who sent and who let
 * it go, the attempts, and each recipient's state. Laid out like the
 * facts box of a message (§12.9), inside a system Voice.
 */
export function OutboxFacts({ e, className }: { e: OutboxEntry; className?: string }) {
  const s = outboxState(e.state);
  const rows: [string, React.ReactNode][] = [];
  rows.push([m.send_fact_state(), <span key="state" className={toneText[s.tone]}>{s.label}</span>]);
  rows.push([m.send_fact_from(), <span key="from" className="font-mono">{e.from}</span>]);
  rows.push([m.send_fact_submitted(), m.send_fact_submitted_by({ time: fullTime(e.created_at), who: principal(e.submitted_by) })]);
  if (e.approved_by) rows.push([m.send_fact_approved_by(), principal(e.approved_by)]);
  rows.push([
    m.send_fact_attempts(),
    e.attempts
      ? e.last_outcome
        ? m.send_fact_attempts_last({ n: e.attempts, outcome: e.last_outcome })
        : String(e.attempts)
      : m.send_fact_attempts_none(),
  ]);
  if (e.next_retry_at) rows.push([m.send_fact_next_try(), `${listTime(e.next_retry_at)} (${fullTime(e.next_retry_at)})`]);
  if (e.error) rows.push([m.send_fact_error(), <span key="error" className="font-mono wrap-anywhere">{e.error}</span>]);
  return (
    <div className={cn("flex flex-col gap-1.5", className)}>
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5">
        {rows.map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-muted-foreground">{k}</dt>
            <dd className="min-w-0 wrap-anywhere">{v}</dd>
          </div>
        ))}
      </dl>
      <table className="w-full border-t border-system-border text-left">
        <caption className="sr-only">{m.send_fact_recipients()}</caption>
        <tbody>
          {e.recipients.map((r) => {
            const rs = recipientState(r.state);
            return (
              <tr key={`${r.kind}:${r.addr}`} className="align-baseline">
                <td className="w-8 pt-1 pr-2 text-meta text-muted-foreground uppercase">{r.kind}</td>
                <td className="pt-1 pr-3 font-mono wrap-anywhere">{r.addr}</td>
                <td className={cn("pt-1 text-right whitespace-nowrap", toneText[rs.tone])}>{rs.label}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
