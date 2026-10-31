import { useDrafts } from "@/api/queries";
import type { Draft, DraftState, SessionInfo } from "@/api/types";
import { Skeleton } from "@/components/ui/skeleton";
import { listTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";

import { AuthorMark, HeldMark, OutgoingMark } from "./marks";
import { draftSnippet, isMine, problem } from "./outgoing";

const draftStates: readonly DraftState[] = ["draft", "held", "outbox"];

/** The draft filter in ?st=: comma-separated draft states; draft,held by default (§12.10). */
export function parseDraftFilter(st: string | undefined): DraftState[] {
  const picked = (st ?? "")
    .split(",")
    .filter((s): s is DraftState => (draftStates as readonly string[]).includes(s));
  return picked.length ? picked : ["draft", "held"];
}

/**
 * Drafts, most recently changed first: the viewer's own, and for a
 * person also those agents wrote for them to confirm (§8.9). Rows are the
 * 56px two-line rows of the message list (§12.8).
 */
export function DraftsList({
  session,
  states,
  selected,
  onSelect,
}: {
  session: SessionInfo;
  states: DraftState[];
  selected?: string;
  onSelect: (id: string) => void;
}) {
  const list = useDrafts(states);
  if (list.isLoading) {
    return (
      <div className="flex flex-col gap-4 p-3">
        {Array.from({ length: 4 }, (_, i) => (
          <div key={i} className="flex flex-col gap-1.5">
            <Skeleton className="h-3.5 w-1/3" />
            <Skeleton className="h-3.5 w-4/5" />
          </div>
        ))}
      </div>
    );
  }
  if (list.error) return <p className="p-4 text-destructive">{problem(list.error).text}</p>;
  if (list.items.length === 0)
    return <p className="p-4 text-muted-foreground">{m.send_drafts_empty()}</p>;
  return (
    <div className="min-h-0 flex-1 overflow-y-auto">
      <ul aria-label={m.send_drafts_list_label()}>
        {list.items.map((d) => (
          <li key={d.id} className="h-14">
            <Row
              d={d}
              mine={isMine(d.author, session)}
              selected={d.id === selected}
              onSelect={onSelect}
            />
          </li>
        ))}
      </ul>
      {list.hasMore && (
        <button
          type="button"
          className="w-full py-2 text-small text-muted-foreground hover:text-foreground"
          disabled={list.isFetchingMore}
          onClick={list.loadMore}
        >
          {list.isFetchingMore ? m.send_loading() : m.send_load_more()}
        </button>
      )}
    </div>
  );
}

function Row({
  d,
  mine,
  selected,
  onSelect,
}: {
  d: Draft;
  mine: boolean;
  selected: boolean;
  onSelect: (id: string) => void;
}) {
  const to = [...d.to, ...d.cc, ...d.bcc];
  const snippet = d.smtp ? m.send_draft_smtp_snippet() : draftSnippet(d.body_markdown);
  return (
    <button
      type="button"
      aria-current={selected || undefined}
      onClick={() => onSelect(d.id)}
      className={cn(
        "flex h-full w-full flex-col justify-center gap-0.5 border-b px-3 text-left outline-none hover:bg-muted focus-visible:bg-muted",
        selected &&
          "bg-accent text-accent-foreground shadow-[inset_2px_0_0_var(--color-foreground)] hover:bg-accent",
      )}
    >
      <span className="flex min-w-0 items-center gap-1.5 text-ui">
        {!mine && d.author.kind !== "human" && <AuthorMark author={d.author} />}
        <span className="min-w-0 truncate font-medium">
          {to.length ? (
            to.join(", ")
          ) : (
            <span className="text-muted-foreground">{m.send_no_recipients()}</span>
          )}
        </span>
        <span className="ml-auto shrink-0 pl-2 text-meta text-muted-foreground tabular-nums">
          {listTime(d.updated_at)}
        </span>
      </span>
      <span className="flex min-w-0 items-center gap-2 text-ui">
        <span className="min-w-0 truncate">
          {d.subject || <span className="text-muted-foreground">{m.send_no_subject()}</span>}
          {snippet && <span className="text-muted-foreground"> — {snippet}</span>}
        </span>
        {d.state === "held" && <HeldMark reason={d.hold_reason} className="ml-auto" />}
        {d.state === "outbox" && d.outbox && (
          <OutgoingMark state={d.outbox.state} className="ml-auto" />
        )}
      </span>
    </button>
  );
}
