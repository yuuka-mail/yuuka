import { useQueryClient } from "@tanstack/react-query";
import { PlusIcon, UserIcon, XIcon } from "lucide-react";
import { useState, type ReactNode } from "react";

import { api, data } from "@/api/client";
import type { Cite, Message, Note, SessionInfo } from "@/api/types";
import { ConfirmButton } from "@/components/confirm";
import { Button } from "@/components/ui/button";
import { Voice } from "@/components/voice";
import { listTime } from "@/lib/format";
import { m } from "@/paraglide/messages.js";
import { ProblemLine } from "@/send/marks";
import { isMine, principal, problem, type Abilities } from "@/send/outgoing";

import { CiteLine } from "./proposals";

// Notes (§8.10): a signed paragraph about a message or its thread. An
// agent's note is its reading of the mail, a guess: it speaks in the
// agent's voice and is untrusted (mail injected into the agent may flow
// into it), so it renders as text only and drives nothing. A person's
// note is theirs, signed with their name, in no hue: the colours are for
// the system and agents (§12.5). Every note says who wrote it and cites
// what it relied on.

/** The notes on a message, and a person's own to write. */
export function MessageNotes({
  message,
  session,
  can,
  anchor,
  onOpen,
}: {
  message: Message;
  session: SessionInfo;
  can: Abilities;
  anchor?: string;
  onOpen: (id: string, anchor?: string) => void;
}) {
  const [writing, setWriting] = useState<{ initial?: Note; start: Cite[] }>();
  const notes = message.notes;
  const own = notes.find((n) => isMine(n.author, session) && n.scope === "message");
  if (!notes.length && !can.notes) return null;
  return (
    <section aria-label={m.notes_label()} className="flex flex-col gap-2">
      {notes.map((n) => (
        <NoteCard
          key={n.id}
          n={n}
          message={message.id}
          mine={isMine(n.author, session)}
          canDelete={can.deleteNotes || isMine(n.author, session)}
          onEdit={() => setWriting({ initial: n, start: n.cites })}
          onOpen={onOpen}
        />
      ))}
      {writing ? (
        <NoteEditor
          message={message.id}
          initial={writing.initial}
          start={writing.start}
          onClose={() => setWriting(undefined)}
        />
      ) : (
        can.notes &&
        !own && (
          <Button
            size="xs"
            variant="ghost"
            className="self-start text-muted-foreground"
            // Keeps a selection in the mail, so it can be the note's first cite.
            onMouseDown={(e) => e.preventDefault()}
            onClick={() =>
              setWriting({
                start: [citeFromSelection(message.id) ?? wholeCite(message.id, anchor)],
              })
            }
          >
            <PlusIcon data-icon="inline-start" />
            {m.notes_add()}
          </Button>
        )
      )}
    </section>
  );
}

const authorName = (n: Note) => (n.author.kind === "agent" ? n.author.name : principal(n.author));

function NoteCard({
  n,
  message,
  mine,
  canDelete,
  onEdit,
  onOpen,
}: {
  n: Note;
  message: string;
  mine: boolean;
  canDelete: boolean;
  onEdit: () => void;
  onOpen: (id: string, anchor?: string) => void;
}) {
  const qc = useQueryClient();
  const title = `${n.scope === "thread" ? m.notes_on_thread() : m.notes_on_message()} · ${listTime(n.updated_at)}`;
  const actions = (mine || canDelete) && (
    <span className="flex flex-wrap items-center gap-1.5">
      {mine && (
        <Button size="xs" variant="ghost" onClick={onEdit}>
          {m.notes_edit()}
        </Button>
      )}
      {canDelete && (
        <ConfirmButton
          label={m.notes_delete()}
          confirmLabel={m.notes_delete_confirm()}
          cost={mine ? m.notes_delete_cost_own() : m.notes_delete_cost({ who: authorName(n) })}
          onConfirm={async () => {
            // A person may delete anyone's note on mail they can see; the author names whose.
            await api.DELETE("/api/v1/messages/{id}/note", {
              params: {
                path: { id: message },
                query: { scope: n.scope, author: mine ? undefined : n.author.id },
              },
            });
            void qc.invalidateQueries({ queryKey: ["get", "/api/v1/messages/{id}"] });
          }}
        />
      )}
    </span>
  );
  const body = (
    <div className="flex flex-col gap-1.5">
      <p lang="" className="text-ui whitespace-pre-wrap break-words">
        {n.text}
      </p>
      {n.cites.length > 0 && (
        <ul className="flex flex-col gap-0.5">
          {n.cites.map((c, i) => (
            <CiteLine key={i} c={c} onOpen={onOpen} />
          ))}
        </ul>
      )}
      {actions}
    </div>
  );
  if (n.author.kind === "human") {
    return (
      <PersonNote name={n.author.name} title={title}>
        {body}
      </PersonNote>
    );
  }
  return (
    <Voice speaker={{ kind: "agent", name: authorName(n) }} title={title}>
      {body}
    </Voice>
  );
}

/** A person's note: signed with their name, in the frame's grey; no voice colour. */
function PersonNote({
  name,
  title,
  children,
}: {
  name: string;
  title: string;
  children: ReactNode;
}) {
  return (
    <section className="rounded-md border px-3 py-2 text-small">
      <header className="mb-1 flex items-center gap-2">
        <span className="inline-flex items-center gap-1.5 text-meta font-semibold tracking-wide">
          <UserIcon aria-hidden className="size-3" />
          {name}
        </span>
        <span className="text-muted-foreground">{title}</span>
      </header>
      {children}
    </section>
  );
}

const maxQuote = 1000;
const maxCites = 32;

/** The message, or the passage the reader is at, without a quote. */
function wholeCite(id: string, anchor?: string): Cite {
  return { uri: `yuuka://message/${id}${anchor ? `#${anchor}` : ""}` };
}

/**
 * The text selected in the open message as a cite: the block it starts
 * in (the blocks' ids are their anchors, §8.1) and the text as quoted.
 * Undefined when nothing in the mail's text is selected.
 */
function citeFromSelection(id: string): Cite | undefined {
  const sel = window.getSelection();
  const text = sel?.toString().replace(/\s+/g, " ").trim();
  if (!sel || !text || sel.rangeCount === 0) return undefined;
  const start = sel.getRangeAt(0).startContainer;
  const el = start instanceof Element ? start : start.parentElement;
  const block = el?.closest<HTMLElement>('[id^="part/"]');
  if (!block) return undefined;
  return { uri: `yuuka://message/${id}#${block.id}`, quote: text.slice(0, maxQuote) };
}

/**
 * Writing one's own note: the text, and what it cites. A note must cite
 * (§12.5); it starts with the selected text, or the message, and more
 * passages can be added from a selection.
 */
function NoteEditor({
  message,
  initial,
  start,
  onClose,
}: {
  message: string;
  initial?: Note;
  start: Cite[];
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [text, setText] = useState(initial?.text ?? "");
  const [cites, setCites] = useState<Cite[]>(start);
  const [thread, setThread] = useState(initial?.scope === "thread");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const [noSelection, setNoSelection] = useState(false);
  const save = async () => {
    setBusy(true);
    setErr(undefined);
    try {
      await data(
        api.PUT("/api/v1/messages/{id}/note", {
          params: { path: { id: message } },
          body: { text: text.trim(), cites, scope: thread ? "thread" : "message" },
        }),
      );
      void qc.invalidateQueries({ queryKey: ["get", "/api/v1/messages/{id}"] });
      onClose();
    } catch (x) {
      setErr(x);
    } finally {
      setBusy(false);
    }
  };
  const addSelection = () => {
    const c = citeFromSelection(message);
    setNoSelection(!c);
    if (c && cites.length < maxCites)
      setCites((cs) => [...cs.filter((x) => x.quote || x.uri !== c.uri), c]);
  };
  return (
    <form
      aria-label={m.notes_editor_label()}
      className="flex flex-col gap-1.5 rounded-md border px-3 py-2 text-small"
      onSubmit={(e) => {
        e.preventDefault();
        void save();
      }}
    >
      <textarea
        aria-label={m.notes_text()}
        // oxlint-disable-next-line jsx-a11y/no-autofocus -- opened by a press to write here
        autoFocus
        value={text}
        maxLength={4000}
        rows={3}
        placeholder={m.notes_placeholder()}
        onChange={(e) => setText(e.target.value)}
        className="min-h-16 w-full resize-y rounded-md border border-input bg-background px-2 py-1 text-ui outline-none focus-visible:border-ring"
      />
      <div className="flex flex-col gap-0.5">
        <span className="text-muted-foreground">{m.notes_cites_label()}</span>
        <ul className="flex flex-col gap-0.5">
          {cites.map((c, i) => (
            <li key={`${c.uri} ${i}`} className="flex min-w-0 items-baseline gap-1.5">
              <span className="shrink-0 font-mono text-meta text-muted-foreground">
                {c.uri.replace(/^yuuka:\/\/message\/[0-9A-Za-z]+#?/, "") ||
                  m.proposal_cite_message()}
              </span>
              {c.quote && (
                <span lang="" className="min-w-0 truncate italic">
                  “{c.quote}”
                </span>
              )}
              <button
                type="button"
                aria-label={m.notes_remove_cite()}
                title={m.notes_remove_cite()}
                className="ml-auto inline-flex size-5 shrink-0 items-center justify-center rounded-sm text-muted-foreground hover:bg-muted hover:text-foreground"
                onClick={() => setCites((cs) => cs.filter((_, j) => j !== i))}
              >
                <XIcon className="size-3" />
              </button>
            </li>
          ))}
        </ul>
        <span className="flex flex-wrap items-center gap-2">
          <Button
            size="xs"
            variant="ghost"
            type="button"
            onMouseDown={(e) => e.preventDefault()}
            onClick={addSelection}
            disabled={cites.length >= maxCites}
          >
            {m.notes_cite_selection()}
          </Button>
          {!cites.some((c) => !c.quote && !c.uri.includes("#")) && (
            <Button
              size="xs"
              variant="ghost"
              type="button"
              onClick={() => setCites((cs) => [...cs, wholeCite(message)])}
              disabled={cites.length >= maxCites}
            >
              {m.notes_cite_message()}
            </Button>
          )}
          {noSelection && <span className="text-muted-foreground">{m.notes_no_selection()}</span>}
        </span>
      </div>
      <label className="flex items-center gap-1.5 self-start text-muted-foreground">
        <input
          type="checkbox"
          className="accent-current"
          checked={thread}
          onChange={(e) => setThread(e.target.checked)}
        />
        {m.notes_about_thread()}
      </label>
      <span className="flex flex-wrap items-center gap-2">
        <Button size="xs" type="submit" disabled={busy || !text.trim() || cites.length === 0}>
          {m.notes_save()}
        </Button>
        <Button size="xs" variant="ghost" type="button" onClick={onClose}>
          {m.rule_cancel()}
        </Button>
        {cites.length === 0 && <span className="text-warning">{m.notes_needs_cite()}</span>}
        {err !== undefined && <ProblemLine p={problem(err)} />}
      </span>
    </form>
  );
}
