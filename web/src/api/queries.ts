import { keepPreviousData, useInfiniteQuery, useQuery } from "@tanstack/react-query";

import { listParams, type View } from "@/mail/view";

import type { SessionInfo } from "./auth.gen";
import { getJSON, isUnauthenticated } from "./client";
import type { MessagePage, MessageSummary, Prefix, PrefixList, SearchResult, Thread } from "./types.gen";

export const keys = {
  session: ["session"] as const,
  prefixes: ["prefixes"] as const,
  unclaimed: ["prefixes", "unclaimed"] as const,
  messages: (p: string) => ["messages", p] as const,
  search: (q: string, dg: string) => ["search", q, dg] as const,
  message: (id: string) => ["message", id] as const,
  reading: (id: string) => ["reading", id] as const,
  thread: (id: string) => ["thread", id] as const,
};

/** The viewer, or null when not logged in. */
export function useSession() {
  return useQuery({
    queryKey: keys.session,
    queryFn: async () => {
      try {
        return await getJSON<SessionInfo>("/auth/session");
      } catch (err) {
        if (isUnauthenticated(err)) return null;
        throw err;
      }
    },
    staleTime: 60_000,
  });
}

/**
 * The claimed prefixes and the mail totals, for the sidebar. Unclaimed
 * prefixes run to thousands under a dictionary attack (§12.9); they are
 * paged in their own list.
 */
export function usePrefixes() {
  return useQuery({
    queryKey: keys.prefixes,
    queryFn: () => getJSON<PrefixList>("/api/v1/prefixes?state=claimed"),
    staleTime: 30_000,
  });
}

const pageSize = 100;

/** Unclaimed prefixes, most recently delivered to first. */
export function useUnclaimedPrefixes(enabled = true) {
  const q = useInfiniteQuery({
    queryKey: keys.unclaimed,
    queryFn: ({ pageParam }) =>
      getJSON<PrefixList>(`/api/v1/prefixes?${new URLSearchParams({ state: "discovered", limit: String(pageSize), offset: String(pageParam) })}`),
    initialPageParam: 0,
    getNextPageParam: (last, pages) => {
      const seen = pages.reduce((n, p) => n + p.prefixes.length, 0);
      return seen < last.total && last.prefixes.length > 0 ? seen : undefined;
    },
    staleTime: 30_000,
    enabled,
  });
  return {
    items: q.data?.pages.flatMap((p) => p.prefixes) ?? ([] as Prefix[]),
    total: q.data?.pages[0]?.total,
    hasMore: q.hasNextPage,
    loadMore: () => void q.fetchNextPage(),
    isLoading: q.isLoading,
    isFetchingMore: q.isFetchingNextPage,
    error: q.error,
  };
}

/** A list the virtualised list pages through, from messages or search. */
export interface MailList {
  items: MessageSummary[];
  /** Known total, when the source reports one (search does). */
  total?: number;
  hasMore: boolean;
  loadMore: () => void;
  isLoading: boolean;
  isFetchingMore: boolean;
  error: unknown;
}

export function useMessageList(v: View, enabled: boolean): MailList {
  const lp = listParams(v);
  const params = lp?.toString() ?? "";
  const q = useInfiniteQuery({
    queryKey: keys.messages(params),
    queryFn: ({ pageParam }) => {
      const p = new URLSearchParams(params);
      p.set("limit", String(pageSize));
      if (pageParam) p.set("cursor", pageParam);
      return getJSON<MessagePage>(`/api/v1/messages?${p}`);
    },
    initialPageParam: "",
    getNextPageParam: (last) => last.next_cursor || undefined,
    placeholderData: keepPreviousData,
    enabled: enabled && !!lp,
  });
  return {
    items: q.data?.pages.flatMap((p) => p.messages) ?? [],
    hasMore: q.hasNextPage,
    loadMore: () => void q.fetchNextPage(),
    isLoading: q.isLoading,
    isFetchingMore: q.isFetchingNextPage,
    error: q.error,
  };
}

export function useSearchList(text: string, includeDowngraded: boolean, enabled: boolean): MailList {
  const dg = includeDowngraded ? "include" : "hide";
  const q = useInfiniteQuery({
    queryKey: keys.search(text, dg),
    queryFn: ({ pageParam }) => {
      const p = new URLSearchParams({ q: text, downgraded: dg, limit: String(pageSize), offset: String(pageParam) });
      return getJSON<SearchResult>(`/api/v1/search?${p}`);
    },
    initialPageParam: 0,
    getNextPageParam: (last, pages) => {
      const seen = pages.reduce((n, p) => n + p.hits.length, 0);
      return seen < last.total && last.hits.length > 0 ? seen : undefined;
    },
    placeholderData: keepPreviousData,
    enabled,
  });
  return {
    items: q.data?.pages.flatMap((p) => p.hits) ?? [],
    total: q.data?.pages[0]?.total,
    hasMore: q.hasNextPage,
    loadMore: () => void q.fetchNextPage(),
    isLoading: q.isLoading,
    isFetchingMore: q.isFetchingNextPage,
    error: q.error,
  };
}

export function useThread(id: string | undefined) {
  return useQuery({
    queryKey: keys.thread(id ?? ""),
    queryFn: () => getJSON<Thread>(`/api/v1/threads/${encodeURIComponent(id ?? "")}`),
    enabled: !!id,
    staleTime: 60_000,
  });
}
