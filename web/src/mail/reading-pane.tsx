import { ArrowLeftIcon, PaperclipIcon } from "lucide-react";

import { useThread } from "@/api/queries";
import type { Message, MessageSummary } from "@/api/types.gen";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { system, Voice } from "@/components/voice";
import { fullTime, listTime, sender } from "@/lib/format";
import { cn } from "@/lib/utils";

import { systemFacts, type Tone } from "./facts";
import { ReadingBody } from "./reading-body";
import { useMessage } from "./reading-data";

export function ReadingPane({ id, onBack, onOpen }: { id: string; onBack: () => void; onOpen: (id: string) => void }) {
  const q = useMessage(id);
  return (
    <article className="flex h-full min-h-0 flex-col">
      <div className="flex h-10 shrink-0 items-center border-b px-2 md:hidden">
        <Button variant="ghost" size="sm" onClick={onBack}>
          <ArrowLeftIcon data-icon="inline-start" />
          Back
        </Button>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto">
        {q.data ? (
          <Reading message={q.data} onOpen={onOpen} />
        ) : q.error ? (
          <p className="p-6 text-sm text-destructive">{q.error instanceof Error ? q.error.message : "Could not load the message."}</p>
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

function Reading({ message: m, onOpen }: { message: Message; onOpen: (id: string) => void }) {
  return (
    <div className="flex max-w-[56rem] flex-col gap-4 px-4 py-4 md:px-6">
      <header className="flex flex-col gap-1.5">
        <h1 className="text-lg leading-snug font-semibold break-words">{m.subject || <span className="text-muted-foreground">(no subject)</span>}</h1>
        <div className="flex flex-wrap items-baseline gap-x-2 text-sm">
          <span className="font-medium">{sender(m.from_name, m.from_addr)}</span>
          {m.from_name && <span className="text-muted-foreground">&lt;{m.from_addr}&gt;</span>}
        </div>
        <div className="flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
          <span>to</span>
          {m.recipients.map((r) => (
            <Badge key={r} variant="outline" className="font-mono font-normal">
              {r}
            </Badge>
          ))}
          {m.date && <span className="ml-auto">dated {fullTime(m.date)}</span>}
          {m.has_attach && (
            <span className="inline-flex items-center gap-1">
              <PaperclipIcon className="size-3" aria-hidden />
              attachment
            </span>
          )}
        </div>
      </header>

      <Voice speaker={system}>
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5">
          {systemFacts(m).map((f) => (
            <div key={f.label} className="contents">
              <dt className="text-muted-foreground">{f.label}</dt>
              <dd className="flex flex-wrap gap-x-2 break-all">
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

      <ThreadStrip message={m} onOpen={onOpen} />

      <ReadingBody message={m} />

      {m.message_id && <p className="font-mono text-[11px] break-all text-muted-foreground">Message-ID &lt;{m.message_id}&gt;</p>}
    </div>
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
    <section className="rounded-md border text-xs">
      <header className="border-b px-3 py-1.5 text-muted-foreground">
        Thread · {ms.length} messages · {t.data?.participants.length} participants
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
                s.id === message.id && "bg-accent text-accent-foreground",
              )}
            >
              <span className="w-28 shrink-0 truncate font-medium">{sender(s.from_name, s.from_addr)}</span>
              <span className="min-w-0 flex-1 truncate text-muted-foreground">{s.snippet || s.subject}</span>
              <span className="shrink-0 text-muted-foreground tabular-nums">{listTime(s.received_at)}</span>
            </button>
          </li>
        ))}
      </ol>
    </section>
  );
}
