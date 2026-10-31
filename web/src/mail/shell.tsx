import { useQueryClient } from "@tanstack/react-query";
import { LogOutIcon, PanelLeftIcon, SearchIcon, XIcon } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import type { SessionInfo } from "@/api/auth.gen";
import { postJSON } from "@/api/client";
import { keys, useMessageList, usePrefixes, useSearchList, useUnclaimedPrefixes, type MailList } from "@/api/queries";
import { Logo } from "@/components/logo";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { count } from "@/lib/format";
import { navigate, useLocation } from "@/lib/router";
import { cn } from "@/lib/utils";

import { MessageList } from "./message-list";
import { PrefixPane } from "./prefix-pane";
import { ReadingPane } from "./reading-pane";
import { UnclaimedList } from "./unclaimed-list";
import { knownTotal, parseView, viewHref, type Area, type View } from "./view";

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
    go({ q: undefined, m: undefined, prefix: undefined, dg: false, ...next });
  };

  const searching = !!view.q;
  const unclaimed = !searching && !view.prefix && view.area === "unclaimed";
  const messages = useMessageList(view, !searching);
  const hits = useSearchList(view.q ?? "", view.dg, searching);
  const list = searching ? hits : messages;

  useKeys(list, view, go, searchBox);

  return (
    <div className="flex h-full flex-col">
      <header className="flex h-11 shrink-0 items-center gap-2 border-b px-2 md:px-3">
        <Button variant="ghost" size="icon-sm" className="lg:hidden" aria-label="Prefixes" aria-pressed={showPrefixes} onClick={() => setShowPrefixes((s) => !s)}>
          <PanelLeftIcon />
        </Button>
        <Logo className="hidden w-[12.25rem] sm:inline-flex" />
        <SearchBox ref={searchBox} value={view.q ?? ""} onSearch={(q) => go({ q: q || undefined, m: undefined }, !!view.q === !!q)} />
        <span className="ml-auto hidden truncate text-meta text-muted-foreground sm:inline">{session.principal}</span>
        <Logout />
      </header>

      <div className="grid min-h-0 flex-1 grid-cols-1 md:grid-cols-[22rem_1fr] lg:grid-cols-[13.5rem_24rem_1fr]">
        <aside className="hidden min-h-0 flex-col border-r bg-sidebar lg:flex">
          <PrefixPane view={view} onPick={pick} />
        </aside>

        <section className={cn("min-h-0 flex-col border-r", view.m ? "hidden md:flex" : "flex")}>
          {showPrefixes && (
            <div className="flex min-h-0 flex-1 flex-col bg-sidebar lg:hidden">
              <PrefixPane view={view} onPick={pick} />
            </div>
          )}
          <div className={cn("min-h-0 flex-1 flex-col", showPrefixes ? "hidden lg:flex" : "flex")}>
            <ListHeader view={view} list={list} go={go} pick={pick} />
            {unclaimed ? (
              <UnclaimedList onPick={(prefix) => go({ prefix, m: undefined })} />
            ) : (
              <MessageList list={list} selected={view.m} markNotifications={searching} onSelect={(m) => go({ m }, true)} />
            )}
          </div>
        </section>

        <main className={cn("min-h-0 min-w-0 flex-col", view.m ? "flex" : "hidden md:flex")}>
          {view.m ? (
            <ReadingPane
              key={view.m}
              id={view.m}
              anchor={view.a}
              onAnchor={(a) => go({ a }, true)}
              onBack={() => go({ m: undefined }, true)}
              onOpen={(m) => go({ m }, true)}
            />
          ) : (
            <div className="m-auto text-center text-small text-muted-foreground">
              {unclaimed ? (
                <p>Pick a prefix to list its mail.</p>
              ) : (
                <>
                  <p>No message selected.</p>
                  <p className="mt-1">
                    <Kbd>j</Kbd> <Kbd>k</Kbd> move · <Kbd>/</Kbd> search · <Kbd>Esc</Kbd> close
                  </p>
                </>
              )}
            </div>
          )}
        </main>
      </div>
    </div>
  );
}

function Kbd({ children }: { children: string }) {
  return <kbd className="rounded border bg-muted px-1 font-mono text-meta">{children}</kbd>;
}

const areaTitles: Record<Area, string> = {
  conversations: "Conversations",
  notifications: "Notifications",
  all: "All mail",
  downgraded: "Downgraded",
  unclaimed: "Unclaimed prefixes",
};

const areaHints: Partial<Record<Area, React.ReactNode>> = {
  downgraded: "Mail to unclaimed prefixes, or that failed DMARC. Hidden everywhere else.",
  unclaimed: (
    <>
      Addresses that got mail without being claimed. Claim one with <code className="font-mono">yuuka prefix claim</code>.
    </>
  ),
};

function ListHeader({
  view,
  list,
  go,
  pick,
}: {
  view: View;
  list: MailList;
  go: (next: Partial<View>, replace?: boolean) => void;
  pick: (next: Partial<View>) => void;
}) {
  const browsing = !view.q && !view.prefix;
  const title = view.q ? `“${view.q}”` : view.prefix ? view.prefix : areaTitles[view.area];
  const prefixes = usePrefixes();
  const unclaimed = useUnclaimedPrefixes(browsing && view.area === "unclaimed");
  const total = browsing && view.area === "unclaimed" ? unclaimed.total : (list.total ?? knownTotal(view, prefixes.data));
  const n = total ?? list.items.length;
  const more = total === undefined && list.hasMore;
  // The zones and the review group never show downgraded mail mixed in;
  // all mail and search can.
  const canToggleDowngraded = !!view.q || (browsing && view.area === "all");
  const hint = browsing ? areaHints[view.area] : undefined;
  // Without the sidebar the two zones are a switch over the list, standing
  // in for the title it would repeat.
  const zone = browsing && (view.area === "conversations" || view.area === "notifications");
  return (
    <div className="flex shrink-0 flex-col gap-1.5 border-b px-3 py-2">
      <div className="flex items-baseline gap-2">
        {zone && (
          <ToggleGroup
            size="sm"
            variant="outline"
            spacing={0}
            className="lg:hidden"
            value={[view.area]}
            onValueChange={(v: string[]) => {
              const a = v[0] as Area | undefined;
              if (a) pick({ area: a });
            }}
          >
            <ToggleGroupItem value="conversations" className="h-6 px-2 text-small">
              Conversations
            </ToggleGroupItem>
            <ToggleGroupItem value="notifications" className="h-6 px-2 text-small">
              Notifications
            </ToggleGroupItem>
          </ToggleGroup>
        )}
        <h1 className={cn("min-w-0 truncate font-semibold", view.prefix && "font-mono", zone && "max-lg:sr-only")}>{title}</h1>
        {!list.isLoading && (
          <span className="shrink-0 text-meta text-muted-foreground tabular-nums">
            {count(n)}
            {more && "+"}
          </span>
        )}
        {canToggleDowngraded && (
          <label className="ml-auto flex shrink-0 items-center gap-1.5 self-center text-small text-muted-foreground">
            <input type="checkbox" className="accent-current" checked={view.dg} onChange={(e) => go({ dg: e.target.checked, m: undefined })} />
            downgraded
          </label>
        )}
      </div>
      {hint && <p className="text-small text-muted-foreground">{hint}</p>}
    </div>
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
        placeholder="Search subject, sender, body"
        aria-label="Search mail"
        className="h-7 pr-7 pl-7"
      />
      {text && (
        <button type="button" aria-label="Clear search" onClick={() => setText("")} className="absolute top-1/2 right-1.5 -translate-y-1/2 text-muted-foreground hover:text-foreground">
          <XIcon className="size-3.5" />
        </button>
      )}
    </div>
  );
}

function Logout() {
  const qc = useQueryClient();
  return (
    <Button
      variant="ghost"
      size="icon-sm"
      aria-label="Sign out"
      title="Sign out"
      onClick={async () => {
        await postJSON("/auth/logout").catch(() => undefined);
        qc.clear();
        qc.setQueryData(keys.session, null);
      }}
    >
      <LogOutIcon />
    </Button>
  );
}

/** j/k move through the list, / focuses search, Esc closes the message. */
function useKeys(list: MailList, view: View, go: (next: Partial<View>, replace?: boolean) => void, search: React.RefObject<HTMLInputElement | null>) {
  const state = useRef({ list, view, go });
  useEffect(() => {
    state.current = { list, view, go };
  });
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      const t = e.target as HTMLElement;
      if (e.metaKey || e.ctrlKey || e.altKey || t.closest("input, textarea, [contenteditable]")) return;
      const cur = state.current;
      if (e.key === "/") {
        e.preventDefault();
        search.current?.focus();
      } else if (e.key === "Escape" && cur.view.m) {
        cur.go({ m: undefined }, true);
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
