import {
  ArrowLeftIcon,
  ChevronDownIcon,
  ChevronUpIcon,
  OctagonAlertIcon,
  PaperclipIcon,
  ReplyAllIcon,
  ReplyIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { useState } from "react";

import { useThread } from "@/api/queries";
import type { Address, Facts, Message, MessageSummary, SessionInfo } from "@/api/types";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Signature, system, Voice } from "@/components/voice";
import { fullTime, listTime, sender } from "@/lib/format";
import { cn } from "@/lib/utils";
import { m as msg } from "@/paraglide/messages.js";
import { OutgoingMark, ProblemLine } from "@/send/marks";
import { problem, type Abilities } from "@/send/outgoing";

import { AnnotateBar } from "./annotate";

import { shownExtraction } from "./extracted";
import { ExtractedLine } from "./extracted-line";
import { isQuiet, pulledFrom, sentState, systemFacts, warnings, type Tone } from "./facts";
import { ReadingBody } from "./reading-body";
import { TriageBox } from "./triage-box";
import { MessageNotes } from "./notes";
import { MessageProposals } from "./proposals";
import { useMessage, useReading } from "./reading-data";

interface Props {
  id: string;
  /** The anchor to open on, part/<section>[/para/<n>]. */
  anchor?: string;
  onAnchor: (anchor: string) => void;
  onBack: () => void;
  /** Opens another message, at an anchor when given (a cite). */
  onOpen: (id: string, anchor?: string) => void;
  /**
   * Starts a reply, to all the message's recipients when all is set;
   * absent when the session may not write drafts.
   */
  onReply?: (all: boolean) => void;
  /** Opens an outbox row; absent when the session may not read the outbox. */
  onOpenOutbox?: (id: string) => void;
  session: SessionInfo;
  can: Abilities;
  /** A proposal picked in the proposals list, to mark among this message's. */
  proposal?: string;
  onOpenRule: (id: string) => void;
}

export function ReadingPane({ id, onBack, ...rest }: Props) {
  const q = useMessage(id);
  return (
    <article className="flex h-full min-h-0 flex-col">
      <div className="flex h-11 shrink-0 items-center border-b px-2 md:hidden">
        <Button variant="ghost" size="sm" onClick={onBack}>
          <ArrowLeftIcon data-icon="inline-start" />
          {msg.reading_back()}
        </Button>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto">
        {q.data ? (
          <Reading message={q.data} {...rest} />
        ) : q.error ? (
          <p className="p-6">
            <ProblemLine p={problem(q.error)} />
          </p>
        ) : (
          <div className="flex flex-col gap-3 p-6">
            <Skeleton className="h-6 w-2/3" />
            <Skeleton className="h-4 w-1/3" />
            <Skeleton className="mt-4 h-24 w-full" />
          </div>
        )}
      </div>
    </article>
  );
}

const toneClass: Record<Tone, string> = {
  ok: "text-system-foreground",
  bad: "text-destructive font-medium",
  warn: "text-warning font-medium",
  plain: "text-foreground",
};

function Reading({
  message: m,
  anchor,
  onAnchor,
  onOpen,
  onReply,
  onOpenOutbox,
  session,
  can,
  proposal,
  onOpenRule,
}: { message: Message } & Omit<Props, "id" | "onBack">) {
  const body = useReading(m.id);
  const facts = body.reading?.facts;
  return (
    <div className="flex max-w-[56rem] flex-col gap-4 px-4 py-4 md:px-6">
      <header className="flex flex-col gap-1.5">
        <div className="flex items-start gap-2">
          <h1 className="min-w-0 flex-1 text-title font-semibold break-words">
            {m.subject ? (
              <span lang="">{m.subject}</span>
            ) : (
              <span className="text-muted-foreground">{msg.no_subject()}</span>
            )}
          </h1>
          {onReply && (
            <Button
              variant="outline"
              size="sm"
              className="shrink-0"
              title={msg.reading_reply_title()}
              onClick={() => onReply(false)}
            >
              <ReplyIcon data-icon="inline-start" />
              {msg.reading_reply()}
            </Button>
          )}
          {/* Only when it would add someone: otherwise it is Reply. */}
          {onReply && m.reply.cc.length > 0 && (
            <Button
              variant="outline"
              size="sm"
              className="shrink-0"
              aria-label={msg.reading_reply_all()}
              title={msg.reading_reply_all_title()}
              onClick={() => onReply(true)}
            >
              <ReplyAllIcon data-icon="inline-start" />
              <span className="max-sm:sr-only">{msg.reading_reply_all()}</span>
            </Button>
          )}
        </div>
        <div className="flex flex-wrap items-baseline gap-x-2 text-body">
          <span className="font-medium">{sender(m.from_name, m.from_addr)}</span>
          {m.from_name && <span className="text-muted-foreground">&lt;{m.from_addr}&gt;</span>}
        </div>
        <HeaderAddresses message={m} />
        <div className="flex flex-wrap items-center gap-1.5 text-small text-muted-foreground">
          {/* Mail we only sent has no delivery here, so no prefix it came to. */}
          {m.recipients.length > 0 && <span>{msg.reading_delivered_to()}</span>}
          {m.recipients.map((r) => (
            <Badge key={r} variant="outline" className="font-mono font-normal">
              {r}
            </Badge>
          ))}
          {/* Pulled from a mailbox (§9.5): which one, next to where it was delivered. */}
          {pulledFrom(m).map((p) => (
            <span key={p.account} className="inline-flex items-center gap-1">
              {msg.reading_pulled_from()}
              <Badge variant="secondary" className="font-mono font-normal">
                {p.account}
              </Badge>
            </span>
          ))}
          {m.date && (
            <span className="ml-auto">{msg.reading_dated({ when: fullTime(m.date) })}</span>
          )}
          {m.has_attach && (
            <span className="inline-flex items-center gap-1">
              <PaperclipIcon className="size-3" aria-hidden />
              {msg.reading_attachment()}
            </span>
          )}
        </div>
      </header>

      <AnnotateBar message={m} can={can} onOpen={onOpen} onOpenRule={onOpenRule} />

      <SystemFacts message={m} facts={facts} onOpenOutbox={onOpenOutbox} />

      <ExtractedBox message={m} />

      <TriageBox score={m.triage_score} />

      <MessageProposals id={m.id} highlight={proposal} can={can} onOpen={onOpen} />

      <MessageNotes message={m} session={session} can={can} anchor={anchor} onOpen={onOpen} />

      <ThreadStrip message={m} onOpen={onOpen} />

      {body.reading ? (
        <ReadingBody
          id={m.id}
          uri={`yuuka://message/${m.id}`}
          reading={body.reading}
          anchor={anchor}
          onAnchor={onAnchor}
          loadMore={body.loadMore}
          isFetchingMore={body.isFetchingMore}
        />
      ) : body.error ? (
        <ProblemLine p={problem(body.error)} />
      ) : (
        <div className="flex flex-col gap-2">
          <Skeleton className="h-4 w-full" />
          <Skeleton className="h-4 w-5/6" />
          <Skeleton className="h-4 w-2/3" />
        </div>
      )}

      {m.message_id && (
        <p className="font-mono text-meta break-all text-muted-foreground">
          Message-ID &lt;{m.message_id}&gt;
        </p>
      )}
    </div>
  );
}

/**
 * The code of a notification, above its text (§12.9): yuuka says what it
 * found, in one line, so the code is at hand without reading the mail.
 */
function ExtractedBox({ message: m }: { message: Message }) {
  const x = shownExtraction(m);
  if (!x) return null;
  return (
    <section
      aria-label={msg.reading_extracted_label()}
      className="flex max-w-full min-w-0 items-center gap-2 self-start rounded-md border border-system-border bg-system-surface px-2 py-0.5"
    >
      <Signature speaker={system} />
      <ExtractedLine x={x} verified={m.sender_verified} mark={false} />
    </section>
  );
}

/** How many addresses of one field show before "+n more". */
const addrsShown = 3;

/**
 * To, Cc and Reply-To as the sender wrote them, names first: one compact
 * line, long lists cut to a few with the rest a click away. These are
 * header text, unverified like the rest of it; where the mail actually
 * arrived is the delivered-to line below.
 */
function HeaderAddresses({ message: m }: { message: Message }) {
  const [all, setAll] = useState(false);
  const fields = [
    [msg.reading_to(), m.to],
    [msg.reading_cc(), m.cc],
    [msg.reading_reply_to(), m.reply_to],
  ] as const;
  if (fields.every(([, l]) => l.length === 0)) return null;
  const hidden = fields.reduce((n, [, l]) => n + Math.max(0, l.length - addrsShown), 0);
  return (
    <div className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 text-small">
      {fields.map(
        ([label, l]) =>
          l.length > 0 && (
            <span key={label} className="min-w-0 break-words">
              <span className="text-muted-foreground">{label} </span>
              {(all ? l : l.slice(0, addrsShown)).map((a, i) => (
                <span key={`${i}:${a.addr}`}>
                  {i > 0 && <span className="text-muted-foreground">, </span>}
                  <AddressText a={a} />
                </span>
              ))}
            </span>
          ),
      )}
      {hidden > 0 && !all && (
        <button
          type="button"
          className="text-muted-foreground underline underline-offset-2 hover:text-foreground"
          onClick={() => setAll(true)}
        >
          {msg.reading_more({ count: hidden })}
        </button>
      )}
    </div>
  );
}

function AddressText({ a }: { a: Address }) {
  if (!a.name) return <span className="font-mono">{a.addr}</span>;
  return (
    <span title={a.addr}>
      {a.name} <span className="text-muted-foreground">&lt;{a.addr}&gt;</span>
    </span>
  );
}

/**
 * What yuuka established about the message: one line when there is
 * nothing to act on (§12.9), the full box when there is or when asked.
 * Until the view loads the line shows only what is already known, so
 * quiet mail, the common case, opens without the box flashing up.
 */
function SystemFacts({
  message: m,
  facts,
  onOpenOutbox,
}: {
  message: Message;
  facts?: Facts;
  onOpenOutbox?: (id: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const sent = facts?.sent;
  if (!open && (!facts || isQuiet(m, facts))) {
    return (
      <button
        type="button"
        aria-expanded={false}
        disabled={!facts}
        onClick={() => setOpen(true)}
        className="flex max-w-full items-center gap-2 self-start rounded-md border border-system-border bg-system-surface px-2 py-0.5 text-small hover:border-system disabled:cursor-default"
      >
        <Signature speaker={system} />
        {sent ? (
          <span className="truncate">
            {msg.reading_sent_as({ from: sent.from })} ·{" "}
            <span className={toneClass[sentState(sent).tone]}>{sentState(sent).text}</span>
          </span>
        ) : (
          facts && (
            <span className="truncate">
              <span className="text-system-foreground">dmarc=pass</span> {facts.auth.from_domain}
              {/* Quiet mail only ever has ok verdicts: a leak or spoof warns. */}
              {facts.leak_checks?.length ? (
                <span className="text-muted-foreground"> · {msg.reading_expected_sender()}</span>
              ) : null}
            </span>
          )
        )}
        <span className="shrink-0 text-muted-foreground">
          {facts?.auth.not_received
            ? msg.reading_submitted_at({ when: listTime(m.received_at) })
            : msg.reading_received_at({ when: listTime(m.received_at) })}
        </span>
        {facts && <ChevronDownIcon aria-hidden className="size-3 shrink-0 text-muted-foreground" />}
      </button>
    );
  }
  const warns = warnings(m, facts);
  // Quiet facts opened by hand can fold back into their line.
  const fold = isQuiet(m, facts) ? (
    <button
      type="button"
      aria-expanded
      aria-label={msg.reading_fold_facts()}
      onClick={() => setOpen(false)}
      className="flex text-muted-foreground hover:text-foreground"
    >
      <ChevronUpIcon aria-hidden className="size-3" />
    </button>
  ) : (
    sent &&
    onOpenOutbox && (
      <button
        type="button"
        className="text-small underline underline-offset-2"
        onClick={() => onOpenOutbox(sent.outbox_id)}
      >
        {msg.reading_open_in_outbox()}
      </button>
    )
  );
  return (
    <Voice
      speaker={system}
      action={fold}
      className={cn(warns.some((w) => w.tone === "bad") && "border-destructive/60")}
    >
      {warns.length > 0 && (
        <ul className="mb-1.5 flex flex-col gap-0.5 border-b border-system-border pb-1.5">
          {warns.map((w) => (
            <li
              key={`${w.key} ${w.text}`}
              className={cn(
                "flex items-baseline gap-1.5",
                w.tone === "bad" ? "text-destructive" : "text-warning",
              )}
            >
              {w.tone === "bad" ? (
                <OctagonAlertIcon aria-hidden className="size-3 shrink-0 self-center" />
              ) : (
                <TriangleAlertIcon aria-hidden className="size-3 shrink-0 self-center" />
              )}
              <span>
                <span className="font-semibold">{w.key}</span>
                {w.text && <span className="text-foreground/80"> · {w.text}</span>}
              </span>
            </li>
          ))}
        </ul>
      )}
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5">
        {systemFacts(m, facts).map((f) => (
          <div key={f.label} className="contents">
            <dt className="text-muted-foreground">{f.label}</dt>
            <dd className="flex flex-wrap gap-x-2 wrap-anywhere">
              {f.parts.map((p, i) => (
                <span key={i} className={toneClass[p.tone]}>
                  {p.text}
                </span>
              ))}
            </dd>
          </div>
        ))}
      </dl>
    </Voice>
  );
}

// Centres the open message in the strip without scrolling the page, which
// scrollIntoView would also do.
function centre(li: HTMLLIElement | null) {
  const ol = li?.parentElement;
  if (li && ol) ol.scrollTop = li.offsetTop - (ol.clientHeight - li.clientHeight) / 2;
}

/** The other messages of the thread, when there are any. */
function ThreadStrip({ message, onOpen }: { message: Message; onOpen: (id: string) => void }) {
  const t = useThread(message.thread_id);
  const ms = t.data?.messages ?? [];
  if (ms.length < 2) return null;
  return (
    <section className="rounded-md border text-small">
      <header className="border-b px-3 py-1.5 text-muted-foreground">
        {msg.reading_thread({
          messages: msg.reading_n_messages({ count: ms.length }),
          participants: msg.reading_n_participants({ count: t.data?.participants.length ?? 0 }),
        })}
      </header>
      {/* Mailing-list threads run long; the strip stays a strip. */}
      <ol className="relative max-h-56 overflow-y-auto">
        {ms.map((s: MessageSummary) => (
          <li key={s.id} ref={s.id === message.id ? centre : undefined}>
            <button
              type="button"
              onClick={() => onOpen(s.id)}
              className={cn(
                "flex w-full items-baseline gap-2 px-3 py-1 text-left hover:bg-muted",
                s.id === message.id &&
                  "bg-accent text-accent-foreground shadow-[inset_2px_0_0_var(--color-foreground)]",
              )}
            >
              <span className="w-28 shrink-0 truncate font-medium">
                {sender(s.from_name, s.from_addr)}
              </span>
              <span className="min-w-0 flex-1 truncate text-muted-foreground">
                {s.snippet || s.subject}
              </span>
              {s.outgoing && <OutgoingMark state={s.outgoing.state} />}
              <span className="shrink-0 text-muted-foreground tabular-nums">
                {listTime(s.received_at)}
              </span>
            </button>
          </li>
        ))}
      </ol>
    </section>
  );
}
