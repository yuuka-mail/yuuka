import { keepPreviousData, useQuery } from "@tanstack/react-query";

import { listParams, type View } from "@/mail/view";

import { $api, api, isUnauthenticated } from "./client";
import type { MessageSummary, Prefix } from "./types";

export const keys = {
  /** The key of useSession, which a 401 anywhere sets to null. */
  session: ["get", "/auth/session"] as const,
};

/** The viewer, or null when not logged in. */
export function useSession() {
  return useQuery({
    queryKey: keys.session,
    queryFn: async ({ signal }) => {
      try {
        const { data } = await api.GET("/auth/session", { signal });
        return data ?? null;
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
  return $api.useQuery("get", "/api/v1/prefixes", { params: { query: { state: "claimed" } } }, { staleTime: 30_000 });
}

const pageSize = 100;

/**
 * Unclaimed prefixes, most recently delivered to first. They change
 * slowly and only people page through them, so they page by offset.
 */
export function useUnclaimedPrefixes(enabled = true) {
  const q = $api.useInfiniteQuery(
    "get",
    "/api/v1/prefixes",
    { params: { query: { state: "discovered", limit: pageSize } } },
    {
      pageParamName: "offset",
      initialPageParam: 0,
      getNextPageParam: (last, pages) => {
        const seen = pages.reduce((n, p) => n + p.prefixes.length, 0);
        return seen < last.total && last.prefixes.length > 0 ? seen : undefined;
      },
      staleTime: 30_000,
      enabled,
    },
  );
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

// Messages and search hits page by cursor: new mail joins at the top, so
// an offset would repeat or skip rows (§5.1). The first page has none.
const cursorPaging = {
  pageParamName: "cursor",
  initialPageParam: "",
  getNextPageParam: (last: { next_cursor?: string }) => last.next_cursor || undefined,
  placeholderData: keepPreviousData,
};

export function useMessageList(v: View, enabled: boolean): MailList {
  const query = listParams(v);
  const q = $api.useInfiniteQuery(
    "get",
    "/api/v1/messages",
    { params: { query: { ...query, limit: pageSize } } },
    { ...cursorPaging, enabled: enabled && !!query },
  );
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
  const q = $api.useInfiniteQuery(
    "get",
    "/api/v1/search",
    { params: { query: { q: text, downgraded: includeDowngraded ? "include" : "hide", limit: pageSize } } },
    { ...cursorPaging, enabled },
  );
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
  return $api.useQuery(
    "get",
    "/api/v1/threads/{id}",
    { params: { path: { id: id ?? "" } } },
    { enabled: !!id, staleTime: 60_000 },
  );
}
