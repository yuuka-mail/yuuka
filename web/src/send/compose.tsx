import { ParaglideMessage } from "@inlang/paraglide-js-react";
import { useQueryClient } from "@tanstack/react-query";
import { ArrowLeftIcon, ExternalLinkIcon, SendIcon, Trash2Icon, XIcon } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { markup } from "@/components/markup";
import { $api, api, data } from "@/api/client";
import { useDraft, useIdentities, useOutboxEntry, useReplyIdentities } from "@/api/queries";
import type {
  Cite,
  Draft,
  HoldReason,
  Message,
  ReplyIdentity,
  Review,
  SendingIdentity,
  SessionInfo,
  SubmitResult,
  Withdrawal,
} from "@/api/types";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Signature, system, Voice } from "@/components/voice";
import { listTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import { useMessage } from "@/mail/reading-data";
import { m as msg } from "@/paraglide/messages.js";

import { AuthorMark, OutgoingMark, ProblemLine } from "./marks";
import { OutboxFacts } from "./outbox-facts";
import {
  abilities,
  badAddrs,
  draftBody,
  goesToReview,
  holdLabel,
  identityNote,
  isMine,
  onlyConfirmer,
  principal,
  problem,
  replyBody,
  replySubject,
  standing,
  strangerStanding,
} from "./outgoing";
import { HoldUntil, Recall, Recalled, ReviewVerdict } from "./review";

// The composer (§12.1, §12.10): Markdown in a textarea, saved as a draft
// as it is typed (create once, then PUT the whole draft), and Send, which
// submits the draft and shows what yuuka decided. A held draft is a
// normal outcome (§8.9), told in the system voice, never an error.
//
// §12.1 picks CodeMirror for the editor; until it comes, the textarea
// carries no paste or key handling beyond Ctrl+Enter, so nothing has to
// be rewritten when it is swapped in.

interface Props {
  session: SessionInfo;
  /** The draft to open; changes to the created one after the first save. */
  draftId?: string;
  /** The message a new reply answers. */
  replyTo?: string;
  onCreated: (id: string) => void;
  onClose: () => void;
  onDeleted: () => void;
  onOpenMessage: (id: string, anchor?: string) => void;
  onOpenOutbox: (id: string) => void;
}

interface Fields {
  from: string;
  to: string;
  cc: string;
  bcc: string;
  subject: string;
  body: string;
  inReplyTo?: string;
}

/** Loads what the composer starts from, then hands it to the editor once. */
export function Composer(props: Props) {
  const opened = props.draftId;
  const ids = useIdentities();
  const draft = useDraft(opened);
  const replied = useMessage(props.replyTo ?? "", !!props.replyTo && !opened);
  const cands = useReplyIdentities(!opened ? props.replyTo : undefined);
  // The first load decides the fields; refetches do not reset them, and
  // neither does the URL naming the draft once the first save created it
  // (the shell keeps this composer mounted across that change).
  const [start, setStart] = useState<{ fields: Fields; draft?: Draft }>();

  const ready =
    ids.data && (opened ? draft.data : props.replyTo ? replied.data && cands.data : true);
  if (!start && ready) {
    const identities = ids.data?.identities ?? [];
    if (opened && draft.data) setStart({ fields: fieldsOf(draft.data), draft: draft.data });
    else if (props.replyTo && replied.data)
      setStart({ fields: replyFields(replied.data, cands.data?.candidates ?? [], identities) });
    else
      setStart({
        fields: { from: defaultFrom(identities), to: "", cc: "", bcc: "", subject: "", body: "" },
      });
  }

  const error = ids.error ?? draft.error ?? replied.error ?? cands.error;
  return (
    <section aria-label={msg.send_compose_label()} className="flex h-full min-h-0 flex-col">
      {start ? (
        <Editor
          {...props}
          initial={start.fields}
          initialDraft={start.draft}
          identities={ids.data?.identities ?? []}
          candidates={cands.data?.candidates ?? []}
        />
      ) : error ? (
        <ComposeError err={error} onClose={props.onClose} />
      ) : (
        <div className="flex flex-col gap-3 p-6">
          <Skeleton className="h-5 w-1/3" />
          <Skeleton className="h-4 w-2/3" />
          <Skeleton className="mt-4 h-40 w-full" />
        </div>
      )}
    </section>
  );
}

function ComposeError({ err, onClose }: { err: unknown; onClose: () => void }) {
  const p = problem(err);
  return (
    <div className="flex flex-col items-start gap-2 p-6">
      <ProblemLine p={p} />
      <Button variant="outline" size="sm" onClick={onClose}>
        {msg.send_close()}
      </Button>
    </div>
  );
}

const join = (xs: string[]) => xs.join(", ");

function fieldsOf(d: Draft): Fields {
  return {
    from: d.from,
    to: join(d.to),
    cc: join(d.cc),
    bcc: join(d.bcc),
    subject: d.subject,
    body: d.body_markdown,
    inReplyTo: d.in_reply_to,
  };
}

/** A pattern identity (*@zone) is not an address; From needs one. */
const isPattern = (a: string) => a.startsWith("*@");

function defaultFrom(ids: SendingIdentity[]): string {
  const concrete = ids.filter((i) => !isPattern(i.address));
  return (concrete.find((i) => i.sends || goesToReview(i)) ?? concrete[0])?.address ?? "";
}

/**
 * A reply: to the sender, Re: the subject, the text quoted below room to
 * write, from the address the message came in through (§9.3) when an
 * identity covers it.
 */
function replyFields(m: Message, cands: ReplyIdentity[], ids: SendingIdentity[]): Fields {
  return {
    from: cands[0]?.from ?? defaultFrom(ids),
    to: m.from_addr,
    cc: "",
    bcc: "",
    subject: replySubject(m.subject),
    body: replyBody(m),
    inReplyTo: m.id,
  };
}

/** The identity that covers address a: its own, else its zone's pattern. */
function identityFor(
  a: string,
  ids: SendingIdentity[],
  cands: ReplyIdentity[],
): SendingIdentity | undefined {
  const lower = a.toLowerCase();
  const exact =
    cands.find((c) => c.from.toLowerCase() === lower)?.identity ??
    ids.find((i) => i.address.toLowerCase() === lower);
  if (exact) return exact;
  const zone = lower.slice(lower.lastIndexOf("@") + 1);
  return ids.find((i) => i.address.toLowerCase() === `*@${zone}`);
}

type SaveState =
  | { s: "idle" }
  | { s: "saving" }
  | { s: "saved"; at: string }
  | { s: "error"; err: unknown };

function Editor({
  session,
  initial,
  initialDraft,
  identities,
  candidates,
  onCreated,
  onClose,
  onDeleted,
  onOpenMessage,
  onOpenOutbox,
}: Props & {
  initial: Fields;
  initialDraft?: Draft;
  identities: SendingIdentity[];
  candidates: ReplyIdentity[];
}) {
  const qc = useQueryClient();
  const can = abilities(session);
  const [f, setF] = useState(initial);
  const [id, setId] = useState(initialDraft?.id);
  const draftQ = useDraft(id);
  const d = draftQ.data ?? initialDraft;
  const [save, setSave] = useState<SaveState>({ s: "idle" });
  const [result, setResult] = useState<SubmitResult>();
  const [sending, setSending] = useState(false);
  const [sendErr, setSendErr] = useState<unknown>();
  const [showCc, setShowCc] = useState(!!(initial.cc || initial.bcc));
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [recalled, setRecalled] = useState<{ w: Withdrawal; identity: string }>();

  // Saving is a chain: each save waits for the one before, so a create
  // is never raced by a PUT, and the newest fields always win.
  const latest = useRef({ f, id });
  const dirty = useRef(false);
  const chain = useRef<Promise<unknown>>(Promise.resolve());
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const alive = useRef(true);
  useEffect(() => {
    // A created id may be ahead of the state that renders it.
    latest.current = { f, id: id ?? latest.current.id };
  });

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ["get", "/api/v1/drafts"] });
  };

  // Saves the newest fields if anything changed since the last save.
  const saveNow = async () => {
    if (!dirty.current) return;
    const { f: cur, id: curId } = latest.current;
    // A new thread needs a From; until there is one there is nothing to save.
    if (!curId && !cur.from && !cur.inReplyTo) return;
    dirty.current = false;
    setSave({ s: "saving" });
    try {
      const body = draftBody(cur);
      const saved = curId
        ? await data(api.PUT("/api/v1/drafts/{id}", { params: { path: { id: curId } }, body }))
        : await data(api.POST("/api/v1/drafts", { body }));
      qc.setQueryData(
        $api.queryOptions("get", "/api/v1/drafts/{id}", { params: { path: { id: saved.id } } })
          .queryKey,
        saved,
      );
      if (!curId) {
        latest.current.id = saved.id;
        setId(saved.id);
        // A composer closed before its first save leaves the URL alone.
        if (alive.current) onCreated(saved.id);
      }
      setSave({ s: "saved", at: saved.updated_at });
      refresh();
    } catch (err) {
      dirty.current = true;
      setSave({ s: "error", err });
      throw err;
    }
  };
  const persist = () => {
    const next = chain.current.then(saveNow, saveNow);
    chain.current = next.catch(() => undefined);
    return next;
  };
  const persistRef = useRef(persist);
  useEffect(() => {
    persistRef.current = persist;
  });

  // Leaving with unsaved typing saves it on the way out.
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
      if (timer.current) {
        clearTimeout(timer.current);
        void persistRef.current().catch(() => undefined);
      }
    };
  }, []);

  const edit = (patch: Partial<Fields>) => {
    setF((cur) => ({ ...cur, ...patch }));
    setResult(undefined);
    setSendErr(undefined);
    dirty.current = true;
    clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      timer.current = undefined;
      void persistRef.current().catch(() => undefined);
    }, 800);
  };

  // Held for the recall window is in the outbox too (§8.9): it goes by itself.
  const inOutbox = d?.state === "outbox" || result?.state === "queued" || result?.state === "held";
  // An SMTP submission's bytes are what its client sent (§13.6).
  const readOnly = inOutbox || !!d?.smtp;
  const confirming = !!d && !isMine(d.author, session);
  const identity =
    identityFor(f.from, identities, candidates) ?? identities.find((i) => i.id === d?.identity_id);
  // The draft's From as saved, when no identity the viewer may use
  // covers it: the draft itself names whose it is (§8.9).
  const stranger =
    !identity && d?.identity_id && d.from.toLowerCase() === f.from.toLowerCase()
      ? strangerStanding(d.identity_owner, session)
      : undefined;
  const bad = [...badAddrs(f.to), ...badAddrs(f.cc), ...badAddrs(f.bcc)];
  const noRecipients = !f.to.trim() && !f.cc.trim() && !f.bcc.trim();
  const canSend =
    can.compose &&
    !inOutbox &&
    !sending &&
    !!f.from &&
    !isPattern(f.from) &&
    bad.length === 0 &&
    !noRecipients;

  const send = async () => {
    if (!canSend) return;
    setSending(true);
    setSendErr(undefined);
    try {
      clearTimeout(timer.current);
      timer.current = undefined;
      // A reply opened and sent untouched has no draft yet.
      if (!latest.current.id) dirty.current = true;
      await persist();
      const draftId = latest.current.id;
      if (!draftId) throw new Error("no draft");
      const r = await data(
        api.POST("/api/v1/drafts/{id}/submit", { params: { path: { id: draftId } } }),
      );
      setResult(r);
      refresh();
      void qc.invalidateQueries({ queryKey: ["get", "/api/v1/drafts/{id}"] });
      if (r.state !== "draft") {
        void qc.invalidateQueries({ queryKey: ["get", "/api/v1/outbox"] });
        void qc.invalidateQueries({ queryKey: ["get", "/api/v1/messages"] });
        void qc.invalidateQueries({ queryKey: ["get", "/api/v1/threads/{id}"] });
      }
    } catch (err) {
      setSendErr(err);
    } finally {
      setSending(false);
    }
  };

  const remove = async () => {
    clearTimeout(timer.current);
    timer.current = undefined;
    dirty.current = false;
    await chain.current;
    const curId = latest.current.id;
    if (curId) {
      try {
        await api.DELETE("/api/v1/drafts/{id}", { params: { path: { id: curId } } });
      } catch (err) {
        setSendErr(err);
        setConfirmDelete(false);
        return;
      }
      refresh();
    }
    onDeleted();
  };

  const title = inOutbox
    ? msg.send_title_in_outbox()
    : f.inReplyTo
      ? msg.send_title_reply()
      : d
        ? msg.send_title_draft()
        : msg.send_title_new();
  const outboxId = result?.outbox_id ?? d?.outbox?.outbox_id;
  // The hold, from this submit or from the draft as stored.
  const held =
    result?.state === "draft"
      ? result
      : !result && d?.state === "held"
        ? { hold_reason: d.hold_reason, hold_detail: d.hold_detail }
        : undefined;

  return (
    <>
      <header className="flex h-11 shrink-0 items-center gap-2 border-b px-2 md:px-4">
        <Button
          variant="ghost"
          size="icon-sm"
          className="md:hidden"
          aria-label={msg.send_back()}
          onClick={onClose}
        >
          <ArrowLeftIcon />
        </Button>
        <h1 className="font-semibold">{title}</h1>
        {d && !isMine(d.author, session) && d.author.kind !== "human" && (
          <AuthorMark author={d.author} />
        )}
        <SaveStatus save={save} />
        <span className="ml-auto flex items-center gap-1">
          {!inOutbox &&
            (confirmDelete ? (
              <>
                <Button variant="destructive" size="sm" onClick={() => void remove()}>
                  {msg.send_delete_draft()}
                </Button>
                <Button variant="ghost" size="sm" onClick={() => setConfirmDelete(false)}>
                  {msg.send_keep()}
                </Button>
              </>
            ) : (
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={msg.send_delete_draft()}
                title={msg.send_delete_draft()}
                onClick={() => setConfirmDelete(true)}
              >
                <Trash2Icon />
              </Button>
            ))}
          <Button
            variant="ghost"
            size="icon-sm"
            className="max-md:hidden"
            aria-label={msg.send_close_composer()}
            title={msg.send_close_composer_title()}
            onClick={onClose}
          >
            <XIcon />
          </Button>
        </span>
      </header>

      <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
        <div className="flex flex-col gap-2 px-4 pt-3 pb-2 empty:hidden md:px-6">
          {d && d.author.kind !== "human" && <AgentNote draft={d} onOpenMessage={onOpenMessage} />}
          {recalled && (
            <Voice speaker={system} title={msg.review_title()}>
              <Recalled w={recalled.w} identity={recalled.identity} submitter={session.principal} />
            </Voice>
          )}
          {held && (
            <HeldNote
              reason={held.hold_reason}
              detail={held.hold_detail}
              confirmer={held.hold_reason === "owner" && d ? onlyConfirmer(d, session) : undefined}
              review={held.hold_reason === "review" ? d?.review : undefined}
              onOpenMessage={onOpenMessage}
            />
          )}
          {inOutbox && outboxId && (
            <QueuedNote
              outboxId={outboxId}
              fallback={d?.outbox?.state ?? result?.state}
              holdUntil={result?.hold_until}
              canSeeOutbox={can.outbox}
              canRecall={can.recall}
              from={f.from}
              submitter={session.principal}
              onOpenOutbox={onOpenOutbox}
              onRecalled={(w, grantIdentity) => {
                // Back to a draft that can be edited and sent again; a
                // suspension it tripped stays said above the form.
                if (w.suspended) setRecalled({ w, identity: grantIdentity });
                setResult(undefined);
                void qc.invalidateQueries({ queryKey: ["get", "/api/v1/drafts/{id}"] });
              }}
            />
          )}
        </div>

        <div className="flex flex-col px-4 md:px-6">
          <Row label={msg.send_field_from()}>
            {/* Confirming someone's draft is about that draft as written:
                the picker's previews are for the viewer's own mail. */}
            {readOnly || confirming ? (
              <span className="font-mono">{f.from}</span>
            ) : (
              <FromPicker
                value={f.from}
                identities={identities}
                candidates={candidates}
                stranger={stranger?.note}
                onChange={(from) => edit({ from })}
              />
            )}
          </Row>
          {!inOutbox && (
            <Standing
              identity={identity}
              stranger={stranger}
              confirming={confirming ? d : undefined}
              session={session}
              from={f.from}
              held={held?.hold_reason}
            />
          )}
          <Row label={msg.send_field_to()}>
            <AddrInput
              value={f.to}
              readOnly={readOnly}
              onChange={(to) => edit({ to })}
              label={msg.send_field_to()}
              autoFocus={!initial.to && !readOnly}
            />
            {!showCc && !readOnly && (
              <button
                type="button"
                className="shrink-0 text-small text-muted-foreground hover:text-foreground"
                onClick={() => setShowCc(true)}
              >
                {msg.send_cc_bcc()}
              </button>
            )}
          </Row>
          {showCc && (
            <>
              <Row label={msg.send_field_cc()}>
                <AddrInput
                  value={f.cc}
                  readOnly={readOnly}
                  onChange={(cc) => edit({ cc })}
                  label={msg.send_field_cc()}
                />
              </Row>
              <Row label={msg.send_field_bcc()}>
                <AddrInput
                  value={f.bcc}
                  readOnly={readOnly}
                  onChange={(bcc) => edit({ bcc })}
                  label={msg.send_field_bcc()}
                />
              </Row>
            </>
          )}
          <Row label={msg.send_field_subject()}>
            <input
              aria-label={msg.send_field_subject()}
              value={f.subject}
              readOnly={readOnly}
              onChange={(e) => edit({ subject: e.target.value })}
              className="h-8 min-w-0 flex-1 bg-transparent font-medium outline-none"
            />
          </Row>
          {bad.length > 0 && (
            <p className="py-1 text-small text-warning">
              {msg.send_not_an_address({ list: bad.join(", ") })}
            </p>
          )}
        </div>

        {d?.smtp ? (
          <p className="px-4 py-3 text-small text-muted-foreground md:px-6">
            {msg.send_smtp_note()}
          </p>
        ) : (
          <textarea
            aria-label={msg.send_field_body()}
            value={f.body}
            readOnly={readOnly}
            placeholder={msg.send_body_placeholder()}
            onChange={(e) => edit({ body: e.target.value })}
            onKeyDown={(e) => {
              if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
                e.preventDefault();
                void send();
              }
            }}
            // A reply opens with the caret above the quote.
            ref={(el) => {
              if (el && initial.inReplyTo && !initialDraft && el.dataset.placed !== "1") {
                el.dataset.placed = "1";
                el.focus();
                el.setSelectionRange(0, 0);
              }
            }}
            className={cn(
              "min-h-64 w-full flex-1 resize-none bg-transparent px-4 py-3 text-body outline-none placeholder:text-muted-foreground md:px-6",
              // Text an agent wrote keeps the agent's colour at its edge
              // (§12.5), inset to the text's column: at the pane's edge it
              // would read as the pane divider.
              d &&
                d.author.kind !== "human" &&
                "ml-2 w-auto border-l-2 border-agent-border pl-2 md:ml-4 md:pl-2",
            )}
          />
        )}
      </div>

      {!inOutbox && (
        <footer className="flex shrink-0 flex-wrap items-center gap-x-3 gap-y-1 border-t px-4 py-2 md:px-6">
          <Button size="sm" disabled={!canSend} onClick={() => void send()}>
            <SendIcon data-icon="inline-start" />
            {sending
              ? msg.send_sending()
              : confirming
                ? msg.send_confirm_and_send()
                : msg.send_send()}
          </Button>
          <span className="text-meta text-muted-foreground max-sm:hidden">Ctrl+Enter</span>
          {!can.compose && (
            <span className="text-small text-muted-foreground">
              {msg.send_no_draft_scope({ scope: "draft:create" })}
            </span>
          )}
          {noRecipients && can.compose && (
            <span className="text-small text-muted-foreground">{msg.send_add_recipient()}</span>
          )}
          {sendErr !== undefined && <ProblemLine p={problem(sendErr)} />}
        </footer>
      )}
    </>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex min-h-9 items-center gap-2 border-b">
      <span className="w-14 shrink-0 text-small text-muted-foreground">{label}</span>
      {children}
    </div>
  );
}

function AddrInput({
  value,
  onChange,
  label,
  readOnly,
  autoFocus,
}: {
  value: string;
  onChange: (v: string) => void;
  label: string;
  readOnly?: boolean;
  autoFocus?: boolean;
}) {
  return (
    <input
      aria-label={label}
      value={value}
      readOnly={readOnly}
      // oxlint-disable-next-line jsx-a11y/no-autofocus -- a new message starts at its first empty field
      autoFocus={autoFocus}
      spellCheck={false}
      autoComplete="off"
      placeholder={readOnly ? "" : msg.send_addr_placeholder()}
      aria-invalid={badAddrs(value).length > 0 || undefined}
      onChange={(e) => onChange(e.target.value)}
      className="h-8 min-w-0 flex-1 bg-transparent font-mono text-ui outline-none placeholder:font-sans placeholder:text-muted-foreground aria-invalid:text-warning"
    />
  );
}

/**
 * From: the reply candidates first (the address the mail came in
 * through), then every identity. A pattern identity (*@zone) asks for the
 * local part, since only an address can be a From. A saved From that no
 * usable identity covers stays an option, noted with what it would do
 * (stranger), so the picker never shows an address the draft is not from.
 */
function FromPicker({
  value,
  identities,
  candidates,
  stranger,
  onChange,
}: {
  value: string;
  identities: SendingIdentity[];
  candidates: ReplyIdentity[];
  stranger?: string;
  onChange: (v: string) => void;
}) {
  const opts: { value: string; note: string }[] = [];
  const seen = new Set<string>();
  const add = (v: string, i: SendingIdentity | string) => {
    if (seen.has(v.toLowerCase())) return;
    seen.add(v.toLowerCase());
    opts.push({ value: v, note: typeof i === "string" ? i : identityNote(i) });
  };
  // The pattern picked, while its local part is being typed.
  const [patternChoice, setPatternChoice] = useState<string>();
  const choosingPattern = patternChoice !== undefined;
  for (const c of candidates) add(c.from, c.identity);
  // An address only a pattern covers, as a saved draft or a reply has it.
  const covering = identityFor(value, identities, candidates);
  if (value && !choosingPattern && covering && isPattern(covering.address)) add(value, covering);
  if (value && !choosingPattern && !covering) add(value, stranger ?? msg.send_opt_no_identity());
  for (const i of identities) add(i.address, i);

  if (opts.length === 0) {
    return (
      <span className="text-small text-muted-foreground">
        <ParaglideMessage message={msg.send_no_identities} markup={markup} />
      </span>
    );
  }
  return (
    <span className="flex min-w-0 flex-1 items-center gap-1.5">
      <select
        aria-label={msg.send_field_from()}
        value={choosingPattern ? patternChoice : value}
        onChange={(e) => {
          const v = e.target.value;
          if (isPattern(v)) {
            setPatternChoice(v);
            onChange("");
          } else {
            setPatternChoice(undefined);
            onChange(v);
          }
        }}
        className="h-7 max-w-full min-w-0 rounded-md border border-input bg-background px-1.5 font-mono text-ui outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
      >
        {opts.map((o) => (
          <option key={o.value} value={o.value}>
            {o.value} — {o.note}
          </option>
        ))}
      </select>
      {choosingPattern && (
        <span className="flex min-w-0 items-center font-mono text-ui">
          <input
            aria-label={msg.send_local_part()}
            placeholder={msg.send_local_part_placeholder()}
            className="h-7 w-28 min-w-0 rounded-md border border-input bg-transparent px-1.5 outline-none focus-visible:border-ring"
            onChange={(e) => {
              const l = e.target.value.trim();
              onChange(l ? `${l}@${patternChoice.slice(2)}` : "");
            }}
          />
          <span className="text-muted-foreground">@{patternChoice.slice(2)}</span>
        </span>
      )}
    </span>
  );
}

/**
 * What pressing Send would do, before it is pressed (§12.10): yuuka's
 * preview of the decision, in the system voice. When it would be held
 * for the reason the last submission was (held), the box above already
 * says so and this line is left out.
 */
function Standing({
  identity,
  stranger,
  confirming,
  session,
  from,
  held,
}: {
  identity?: SendingIdentity;
  /** What it would do as the draft's own From, when no usable identity covers it. */
  stranger?: ReturnType<typeof strangerStanding>;
  confirming?: Draft;
  session: SessionInfo;
  from: string;
  held?: HoldReason;
}) {
  let sends: boolean;
  let headline: string;
  let detail = "";
  let reason: HoldReason | undefined;
  if (confirming) {
    // §8.9 rule 2: a person confirming someone else's draft replaces the
    // tier; only the owner may confirm mail as a person's own address.
    const owner = onlyConfirmer(confirming, session);
    sends = !owner;
    reason = owner ? "owner" : undefined;
    headline = owner
      ? msg.send_only_owner_confirms({ name: owner })
      : msg.send_confirms_draft({ name: confirming.author.name });
    detail = owner ? msg.send_owner_cap_detail() : msg.send_confirm_detail();
  } else if (!from || isPattern(from)) {
    return null;
  } else if (!identity && stranger) {
    sends = false;
    ({ headline, detail, reason } = stranger);
  } else if (!identity) {
    sends = false;
    headline = msg.send_no_identity_headline();
    detail = msg.send_no_identity_detail();
  } else {
    ({ sends, headline, detail } = standing(identity));
    reason = sends ? undefined : identity.hold_reason;
  }
  if (!sends && reason && reason === held) return null;
  return (
    <div className="flex min-h-7 flex-wrap items-center gap-x-2 border-b py-1 text-small">
      <Signature speaker={system} />
      <span className={cn("font-medium", sends ? "text-system-foreground" : "text-foreground")}>
        {headline}
      </span>
      {detail && <span className="min-w-0 text-muted-foreground">{detail}</span>}
    </div>
  );
}

/** The agent that wrote the draft, and what it cites (§8.5). */
function AgentNote({
  draft,
  onOpenMessage,
}: {
  draft: Draft;
  onOpenMessage: (id: string, anchor?: string) => void;
}) {
  const name =
    draft.author.kind === "agent" ? draft.author.name : `${draft.author.kind}:${draft.author.name}`;
  return (
    <Voice
      speaker={{ kind: "agent", name }}
      title={msg.send_agent_drafted({ time: listTime(draft.created_at) })}
    >
      {draft.cites.length > 0 ? (
        <ul className="flex flex-col gap-0.5">
          {draft.cites.map((c, i) => (
            <CiteItem key={i} cite={c} onOpen={onOpenMessage} />
          ))}
        </ul>
      ) : (
        <p className="text-muted-foreground">{msg.send_no_cites()}</p>
      )}
    </Voice>
  );
}

const citePattern = /^yuuka:\/\/message\/([0-9A-Za-z]+)(?:#(.+))?$/;

function CiteItem({ cite, onOpen }: { cite: Cite; onOpen: (id: string, anchor?: string) => void }) {
  const hit = citePattern.exec(cite.uri);
  return (
    <li className="flex min-w-0 items-baseline gap-1.5">
      <span className="text-muted-foreground">{msg.send_cites()}</span>
      {cite.quote && <span className="min-w-0 truncate italic">“{cite.quote}”</span>}
      {hit ? (
        <button
          type="button"
          className="inline-flex shrink-0 items-center gap-0.5 underline underline-offset-2"
          onClick={() => onOpen(hit[1] ?? "", hit[2])}
        >
          {hit[2] ?? msg.send_cite_message()}
          <ExternalLinkIcon aria-hidden className="size-3" />
        </button>
      ) : (
        <span className="font-mono">{cite.uri}</span>
      )}
    </li>
  );
}

/**
 * A submission that stopped at the draft, and why (§8.9). confirmer is
 * the one person who may confirm it, when the owner cap says so.
 */
function HeldNote({
  reason,
  detail,
  confirmer,
  review,
  onOpenMessage,
}: {
  reason?: HoldReason;
  detail?: string;
  confirmer?: string;
  /** What the review's checks found, when they escalated it (§8.9). */
  review?: Review;
  onOpenMessage: (id: string) => void;
}) {
  return (
    <Voice speaker={system} title={msg.send_held_title()}>
      <p>
        <span className="font-semibold">{holdLabel(reason)}</span>
        {/* The checks below say it check by check; the detail would say it again. */}
        {detail && !review && <span className="text-muted-foreground"> · {detail}</span>}
      </p>
      {review && (
        <div className="mt-1">
          <ReviewVerdict review={review} onOpenMessage={onOpenMessage} />
        </div>
      )}
      <p className="mt-0.5 text-muted-foreground">
        {confirmer
          ? msg.send_held_nothing_sent_owner({ name: confirmer })
          : msg.send_held_nothing_sent()}
      </p>
    </Voice>
  );
}

/** A submitted draft's outbox row, followed live while it moves. */
function QueuedNote({
  outboxId,
  fallback,
  holdUntil,
  canSeeOutbox,
  canRecall,
  from,
  submitter,
  onOpenOutbox,
  onRecalled,
}: {
  outboxId: string;
  fallback?: string;
  /** The end of the recall window, as the submission reported it. */
  holdUntil?: string;
  canSeeOutbox: boolean;
  canRecall: boolean;
  /** The From and the submitter, for when the outbox row is not in view. */
  from: string;
  submitter: string;
  onOpenOutbox: (id: string) => void;
  /** identity is the address of the identity the grant is on, *@zone for a pattern. */
  onRecalled: (w: Withdrawal, identity: string) => void;
}) {
  const e = useOutboxEntry(outboxId, canSeeOutbox);
  const state = e.data?.state ?? fallback ?? "queued";
  const until = e.data?.hold_until ?? holdUntil;
  return (
    <Voice
      speaker={system}
      title={state === "held" ? msg.review_held_title() : msg.send_submitted_title()}
      action={
        canSeeOutbox && (
          <button
            type="button"
            className="text-small underline underline-offset-2"
            onClick={() => onOpenOutbox(outboxId)}
          >
            {msg.send_open_in_outbox()}
          </button>
        )
      }
    >
      <div className="flex flex-col gap-1.5">
        {state === "held" && until && (
          <p>
            <HoldUntil until={until} />
          </p>
        )}
        {e.data ? (
          <OutboxFacts e={e.data} />
        ) : (
          <p className="flex items-center gap-2">
            <OutgoingMark state={state} /> {msg.send_outbox_id({ id: outboxId })}
          </p>
        )}
        {state === "held" && canRecall && (
          <Recall
            id={outboxId}
            identity={e.data?.identity ?? from}
            submitter={e.data ? principal(e.data.submitted_by) : submitter}
            onRecalled={(w) => onRecalled(w, e.data?.identity ?? from)}
          />
        )}
      </div>
    </Voice>
  );
}

function SaveStatus({ save }: { save: SaveState }) {
  switch (save.s) {
    case "idle":
      return null;
    case "saving":
      return <span className="text-meta text-muted-foreground">{msg.send_saving()}</span>;
    case "saved":
      return (
        <span className="text-meta text-muted-foreground">
          {msg.send_saved({ time: listTime(save.at) })}
        </span>
      );
    case "error":
      return (
        <span className="text-meta font-medium text-destructive">
          {msg.send_not_saved({ reason: problem(save.err).text })}
        </span>
      );
  }
}
