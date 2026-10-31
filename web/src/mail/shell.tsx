import { ParaglideMessage } from "@inlang/paraglide-js-react";
import { PanelLeftIcon, SearchIcon, SquarePenIcon, XIcon } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { markup } from "@/components/markup";
import type { SessionInfo } from "@/api/types";
import { useMessageList, usePrefixes, useSearchList, useUnclaimedPrefixes, type MailList } from "@/api/queries";
import { Logo } from "@/components/logo";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { count } from "@/lib/format";
import { navigate, useLocation } from "@/lib/router";
import { m as msg } from "@/paraglide/messages.js";
import { cn } from "@/lib/utils";
import { Composer } from "@/send/compose";
import { DraftsList, parseDraftFilter } from "@/send/drafts-list";
import { OutboxList, parseOutboxFilter } from "@/send/outbox-list";
import { OutboxPane } from "@/send/outbox-pane";
import { abilities, outboxFilters, type OutboxFilter } from "@/send/outgoing";

import { SettingsPage, settingsFor } from "@/settings/settings-page";

import { AccountMenu } from "./account-menu";
import { areaTitle } from "./areas";

import { LeakList, PrefixFacts, useFlaggedCount } from "./leaks";
import { MessageList } from "./message-list";
import { PrefixPane } from "./prefix-pane";
import { ProposalList, StandaloneProposal } from "./proposals";
import { ReadingPane } from "./reading-pane";
import { UnclaimedList } from "./unclaimed-list";
import { closeComposer, composing, knownTotal, mainOpen, parseView, viewHref, type Area, type View } from "./view";

/**
 * The three columns: prefixes · message list · reading pane. Below lg the
 * prefixes fold into the list column behind a button; below md one
 * column shows at a time, the reading pane over the list.
 */
export function MailShell({ session }: { session: SessionInfo }) {
  const { searchParams } = useLocation();
  const view = parseView(searchParams);
  const [showPrefixes, setShowPrefixes] = useState(false);
  const searchBox = useRef<HTMLInputElement>(null);
  const can = abilities(session);
  const settings = settingsFor(can);
  // The composer stays mounted when its first save puts the new draft's
  // id in the URL: the fields are already in it, mid-typing.
  const [adopted, setAdopted] = useState<{ key: string; id: string }>();
  // Every Compose is a new composer, even over one that is still "new".
  const [fresh, setFresh] = useState(0);
  const openKey = view.d ? `d:${view.d}` : view.reply ? `reply:${view.reply}` : `new:${fresh}`;
  const composerKey = adopted && view.d === adopted.id ? adopted.key : openKey;

  // Opening a message replaces the history entry; changing what the list
  // shows pushes one, so Back returns to the previous list.
  const go = (next: Partial<View>, replace = false) => {
    // An anchor points into one message; opening another drops it.
    const a = "a" in next ? next.a : "m" in next && next.m !== view.m ? undefined : view.a;
    navigate(viewHref({ ...view, ...next, a }), { replace });
  };
  // Picking an area leaves any prefix; picking a prefix keeps the area,
  // so Back from it lands where it was picked.
  const pick = (next: Partial<View>) => {
    setShowPrefixes(false);
    go({ q: undefined, m: undefined, prefix: undefined, label: undefined, dg: false, o: undefined, st: undefined, p: undefined, ...closeComposer, ...next });
  };
  const compose = (next: Partial<View>) => go({ ...closeComposer, o: undefined, ...next });
  const startNew = () => {
    setFresh((n) => n + 1);
    compose({ compose: true });
  };

  const searching = !!view.q;
  const browsing = !searching && !view.prefix && !view.label;
  const unclaimed = browsing && view.area === "unclaimed";
  const leaks = browsing && view.area === "leaks";
  const drafts = browsing && view.area === "drafts";
  const outbox = browsing && view.area === "outbox";
  const proposals = browsing && view.area === "proposals";
  const messages = useMessageList(view, !searching);
  const hits = useSearchList(view.q ?? "", view.dg, searching);
  const list = searching ? hits : messages;

  useKeys(drafts || outbox || unclaimed || leaks || proposals ? noList : list, view, go, searchBox, can.compose ? startNew : undefined);

  return (
    <div className="flex h-full flex-col">
      <header className="flex h-11 shrink-0 items-center gap-2 border-b px-2 md:px-3">
        <Button variant="ghost" size="icon-sm" className="lg:hidden" aria-label={msg.shell_prefixes()} aria-pressed={showPrefixes} onClick={() => setShowPrefixes((s) => !s)}>
          <PanelLeftIcon />
        </Button>
        <Logo className="hidden w-[12.25rem] sm:inline-flex" />
        <SearchBox ref={searchBox} value={view.q ?? ""} onSearch={(q) => go({ q: q || undefined, m: undefined }, !!view.q === !!q)} />
        {can.compose && (
          <Button variant="outline" size="sm" aria-label={msg.shell_compose()} title={msg.shell_compose_title()} onClick={startNew}>
            <SquarePenIcon data-icon="inline-start" />
            <span className="max-sm:sr-only">{msg.shell_compose()}</span>
          </Button>
        )}
        <AccountMenu session={session} className="ml-auto" onSettings={settings[0] ? () => go({ settings: settings[0], rule: undefined }) : undefined} />
      </header>

      {view.settings ? (
        <SettingsPage view={view} can={can} go={go} onOpenMessage={(m) => go({ settings: undefined, rule: undefined, m })} />
      ) : (
      <div className="grid min-h-0 flex-1 grid-cols-1 md:grid-cols-[22rem_1fr] lg:grid-cols-[13.5rem_24rem_1fr]">
        <aside className="hidden min-h-0 flex-col border-r bg-sidebar lg:flex">
          <PrefixPane view={view} can={can} onPick={pick} />
        </aside>

        <section className={cn("min-h-0 flex-col border-r", mainOpen(view) ? "hidden md:flex" : "flex")}>
          {showPrefixes && (
            <div className="flex min-h-0 flex-1 flex-col bg-sidebar lg:hidden">
              <PrefixPane view={view} can={can} onPick={pick} />
            </div>
          )}
          <div className={cn("min-h-0 flex-1 flex-col", showPrefixes ? "hidden lg:flex" : "flex")}>
            <ListHeader view={view} list={list} go={go} pick={pick} managePrefixes={can.prefixes} />
            {unclaimed ? (
              <UnclaimedList canClaim={can.prefixes} onPick={(prefix) => go({ prefix, m: undefined })} />
            ) : leaks ? (
              <LeakList onPick={(prefix) => go({ prefix, m: undefined })} />
            ) : drafts ? (
              <DraftsList session={session} states={parseDraftFilter(view.st)} selected={view.d} onSelect={(d) => compose({ d, m: undefined })} />
            ) : outbox ? (
              <OutboxList filter={parseOutboxFilter(view.st)} selected={view.o} onSelect={(o) => go({ ...closeComposer, o, m: undefined }, true)} />
            ) : proposals ? (
              <ProposalList selected={view.p} onPick={(p) => go({ ...closeComposer, o: undefined, p: p.id, m: p.message_id, a: undefined }, true)} />
            ) : (
              <MessageList list={list} selected={view.m} markNotifications={searching} onSelect={(m) => go({ m }, true)} />
            )}
          </div>
        </section>

        <main className={cn("min-h-0 min-w-0 flex-col", mainOpen(view) ? "flex" : "hidden md:flex")}>
          {composing(view) ? (
            <Composer
              key={composerKey}
              session={session}
              draftId={view.d}
              replyTo={view.reply}
              onCreated={(id) => {
                setAdopted({ key: composerKey, id });
                go({ ...closeComposer, d: id }, true);
              }}
              onClose={() => go(closeComposer, true)}
              onDeleted={() => go(closeComposer, true)}
              onOpenMessage={(m, a) => go({ ...closeComposer, m, a })}
              onOpenOutbox={(o) => go({ ...closeComposer, area: "outbox", prefix: undefined, q: undefined, m: undefined, st: undefined, o })}
            />
          ) : view.o ? (
            <OutboxPane
              key={view.o}
              id={view.o}
              session={session}
              can={can}
              onBack={() => go({ o: undefined }, true)}
              onOpenMessage={(m) => go({ o: undefined, m })}
              onOpenDraft={(d) => go({ o: undefined, area: "drafts", st: "outbox", d })}
            />
          ) : view.m ? (
            <ReadingPane
              key={view.m}
              id={view.m}
              anchor={view.a}
              onAnchor={(a) => go({ a }, true)}
              onBack={() => go({ m: undefined }, true)}
              onOpen={(m, a) => go({ m, a }, true)}
              onReply={can.compose ? () => compose({ reply: view.m }) : undefined}
              onOpenOutbox={can.outbox ? (o) => go({ area: "outbox", prefix: undefined, q: undefined, m: undefined, st: undefined, o }) : undefined}
              can={can}
              proposal={view.p}
              onOpenRule={(rule) => go({ settings: "rules", rule })}
            />
          ) : view.p ? (
            <StandaloneProposal key={view.p} id={view.p} can={can} onBack={() => go({ p: undefined }, true)} onOpen={(m, a) => go({ m, a })} />
          ) : (
            <div className="m-auto text-center text-small text-muted-foreground">
              {unclaimed || leaks ? (
                <p>{msg.shell_pick_prefix()}</p>
              ) : drafts ? (
                <p>{msg.shell_no_draft_open()}</p>
              ) : outbox ? (
                <p>{msg.shell_pick_outbox_row()}</p>
              ) : proposals ? (
                <p>{msg.shell_pick_proposal()}</p>
              ) : (
                <>
                  <p>{msg.shell_no_message()}</p>
                  <p className="mt-1">
                    <ParaglideMessage message={msg.shell_keys} markup={markup} />
                  </p>
                </>
              )}
            </div>
          )}
        </main>
      </div>
      )}
    </div>
  );
}

function areaHint(a: Area): React.ReactNode {
  switch (a) {
    case "downgraded":
      return msg.area_downgraded_hint();
    case "unclaimed":
      return <ParaglideMessage message={msg.area_unclaimed_hint} markup={markup} />;
    case "leaks":
      return <ParaglideMessage message={msg.area_leaks_hint} markup={markup} />;
    default:
      return undefined;
  }
}

function ListHeader({
  view,
  list,
  go,
  pick,
  managePrefixes,
}: {
  view: View;
  list: MailList;
  go: (next: Partial<View>, replace?: boolean) => void;
  pick: (next: Partial<View>) => void;
  /** Claiming, unclaiming and expected domains, beside the prefix's mail (§12.12). */
  managePrefixes: boolean;
}) {
  const browsing = !view.q && !view.prefix && !view.label;
  const title = view.q ? msg.shell_search_title({ q: view.q }) : view.prefix ? view.prefix : view.label ? msg.shell_label_title({ label: view.label }) : areaTitle(view.area);
  const prefixes = usePrefixes();
  const unclaimed = useUnclaimedPrefixes(browsing && view.area === "unclaimed");
  const flagged = useFlaggedCount();
  const total =
    browsing && view.area === "unclaimed"
      ? unclaimed.total
      : browsing && view.area === "leaks"
        ? flagged
        : (list.total ?? knownTotal(view, prefixes.data));
  const n = total ?? list.items.length;
  const more = total === undefined && list.hasMore;
  // The zones and the review group never show downgraded mail mixed in;
  // all mail and search can.
  const canToggleDowngraded = !!view.q || (browsing && view.area === "all");
  const hint = browsing ? areaHint(view.area) : undefined;
  // Without the sidebar the two zones are a switch over the list, standing
  // in for the title it would repeat.
  const zone = browsing && (view.area === "conversations" || view.area === "notifications");
  // Drafts and the outbox are not mail lists: their own lists, own filters.
  const sending = browsing && (view.area === "drafts" || view.area === "outbox" || view.area === "proposals");
  return (
    <div className="flex shrink-0 flex-col gap-1.5 border-b px-3 py-2">
      <div className="flex items-baseline gap-2">
        {zone && (
          <ToggleGroup
            size="xs"
            variant="outline"
            spacing={0}
            className="lg:hidden"
            aria-label={msg.shell_zone()}
            value={[view.area]}
            onValueChange={(v: string[]) => {
              const a = v[0] as Area | undefined;
              if (a) pick({ area: a });
            }}
          >
            <ToggleGroupItem value="conversations">{msg.area_conversations()}</ToggleGroupItem>
            <ToggleGroupItem value="notifications">{msg.area_notifications()}</ToggleGroupItem>
          </ToggleGroup>
        )}
        <h1 className={cn("min-w-0 truncate font-semibold", view.prefix && "font-mono", zone && "max-lg:sr-only")}>{title}</h1>
        {!list.isLoading && !sending && (
          <span className="shrink-0 text-meta text-muted-foreground tabular-nums">
            {count(n)}
            {more && "+"}
          </span>
        )}
        {canToggleDowngraded && (
          <label className="ml-auto flex shrink-0 items-center gap-1.5 self-center text-small text-muted-foreground">
            <input type="checkbox" className="accent-current" checked={view.dg} onChange={(e) => go({ dg: e.target.checked, m: undefined })} />
            {msg.shell_include_downgraded()}
          </label>
        )}
      </div>
      {hint && <p className="text-small text-muted-foreground">{hint}</p>}
      {view.prefix && !view.q && <PrefixFacts address={view.prefix} manage={managePrefixes} onOpen={(m) => go({ m }, true)} />}
      {browsing && view.area === "drafts" && <DraftFilter st={view.st} onChange={(st) => go({ st, d: undefined })} />}
      {browsing && view.area === "outbox" && <OutboxFilterBar st={view.st} onChange={(st) => go({ st, o: undefined })} />}
    </div>
  );
}

const draftFilterLabels = { draft: () => msg.shell_drafts_draft(), held: () => msg.shell_drafts_held(), outbox: () => msg.shell_drafts_outbox() } as const;

/** Which drafts to list: any of draft, held, in the outbox (§12.10). */
function DraftFilter({ st, onChange }: { st?: string; onChange: (st: string | undefined) => void }) {
  const value = parseDraftFilter(st);
  return (
    <ToggleGroup
      multiple
      size="xs"
      variant="outline"
      spacing={0}
      aria-label={msg.shell_show_drafts()}
      value={value}
      onValueChange={(v: string[]) => {
        const next = parseDraftFilter(v.join(","));
        // Draft and held is the default; it stays out of the URL.
        onChange(v.length === 0 || next.join(",") === "draft,held" ? undefined : next.join(","));
      }}
    >
      {(Object.keys(draftFilterLabels) as (keyof typeof draftFilterLabels)[]).map((k) => (
        <ToggleGroupItem key={k} value={k}>
          {draftFilterLabels[k]()}
        </ToggleGroupItem>
      ))}
    </ToggleGroup>
  );
}

function OutboxFilterBar({ st, onChange }: { st?: string; onChange: (st: string | undefined) => void }) {
  return (
    <ToggleGroup
      size="xs"
      variant="outline"
      spacing={0}
      aria-label={msg.shell_show_outbox()}
      value={[parseOutboxFilter(st)]}
      onValueChange={(v: string[]) => {
        const f = v[0] as OutboxFilter | undefined;
        if (f) onChange(f === "all" ? undefined : f);
      }}
    >
      {(Object.keys(outboxFilters) as OutboxFilter[]).map((k) => (
        <ToggleGroupItem key={k} value={k}>
          {outboxFilters[k].label}
        </ToggleGroupItem>
      ))}
    </ToggleGroup>
  );
}

function SearchBox({ ref, value, onSearch }: { ref: React.Ref<HTMLInputElement>; value: string; onSearch: (q: string) => void }) {
  const [text, setText] = useState(value);
  const [lastValue, setLastValue] = useState(value);
  // Follow the URL when it changes underneath (Back, a prefix click).
  if (value !== lastValue) {
    setLastValue(value);
    setText(value);
  }
  const latest = useRef(onSearch);
  useEffect(() => {
    latest.current = onSearch;
  });
  useEffect(() => {
    if (text.trim() === value) return;
    const t = setTimeout(() => latest.current(text.trim()), 300);
    return () => clearTimeout(t);
  }, [text, value]);

  return (
    <div className="relative min-w-0 flex-1 md:max-w-md">
      <SearchIcon aria-hidden className="pointer-events-none absolute top-1/2 left-2 size-3.5 -translate-y-1/2 text-muted-foreground" />
      <Input
        ref={ref}
        type="search"
        value={text}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter") latest.current(text.trim());
          if (e.key === "Escape") e.currentTarget.blur();
        }}
        placeholder={msg.shell_search_placeholder()}
        aria-label={msg.shell_search_label()}
        className="h-7 pr-7 pl-7"
      />
      {text && (
        <button type="button" aria-label={msg.shell_search_clear()} onClick={() => setText("")} className="absolute top-1/2 right-1.5 -translate-y-1/2 text-muted-foreground hover:text-foreground">
          <XIcon className="size-3.5" />
        </button>
      )}
    </div>
  );
}

const noList: MailList = { items: [], hasMore: false, loadMore: () => undefined, isLoading: false, isFetchingMore: false, error: undefined };

/**
 * j/k move through the list, / focuses search, Esc closes what the main
 * column shows, r replies to the open message, c starts a new one.
 */
function useKeys(
  list: MailList,
  view: View,
  go: (next: Partial<View>, replace?: boolean) => void,
  search: React.RefObject<HTMLInputElement | null>,
  startNew: (() => void) | undefined,
) {
  const state = useRef({ list, view, go, startNew });
  useEffect(() => {
    state.current = { list, view, go, startNew };
  });
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      const t = e.target as HTMLElement;
      if (e.metaKey || e.ctrlKey || e.altKey || t.closest("input, textarea, [contenteditable]")) return;
      const cur = state.current;
      if (e.key === "/") {
        e.preventDefault();
        search.current?.focus();
      } else if (e.key === "Escape" && composing(cur.view)) {
        cur.go(closeComposer, true);
      } else if (e.key === "Escape" && cur.view.o) {
        cur.go({ o: undefined }, true);
      } else if (e.key === "Escape" && cur.view.m) {
        cur.go({ m: undefined }, true);
      } else if (e.key === "c" && cur.startNew && !composing(cur.view)) {
        cur.startNew();
      } else if (e.key === "r" && cur.startNew && cur.view.m && !composing(cur.view)) {
        cur.go({ o: undefined, reply: cur.view.m });
      } else if (e.key === "j" || e.key === "k") {
        const i = cur.list.items.findIndex((m) => m.id === cur.view.m);
        const next = cur.list.items[e.key === "j" ? i + 1 : Math.max(i - 1, 0)];
        if (next) cur.go({ m: next.id }, true);
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [search]);
}
