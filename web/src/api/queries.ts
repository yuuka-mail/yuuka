import { keepPreviousData, useQuery } from "@tanstack/react-query";

import { listParams, type View } from "@/mail/view";
import { outboxMoves } from "@/send/outgoing";

import { $api, api, isUnauthenticated } from "./client";
import type { Draft, DraftState, MessageSummary, OutboxEntry, OutboxState, Prefix } from "./types";

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

/**
 * One prefix with its leak check (§3.3) and the latest mail that leaked
 * or spoofed; only claimed prefixes have one.
 */
export function usePrefixDetail(address: string | undefined, enabled = true) {
  return $api.useQuery(
    "get",
    "/api/v1/prefixes/{address}",
    { params: { path: { address: address ?? "" } } },
    { staleTime: 30_000, enabled: enabled && !!address },
  );
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

// Sending (§12.10). Drafts and the outbox change under the viewer's own
// hands and the outbox executor's, so they are never cached for long.

/** The identities the viewer may send as, with how a submission would go. */
export function useIdentities(enabled = true) {
  return $api.useQuery("get", "/api/v1/identities", {}, { staleTime: 30_000, enabled });
}

/** The identities a reply to message id defaults to, best first. */
export function useReplyIdentities(id: string | undefined) {
  return $api.useQuery("get", "/api/v1/messages/{id}/reply-identities", { params: { path: { id: id ?? "" } } }, { enabled: !!id, staleTime: 30_000 });
}

export function useDrafts(state: DraftState[]) {
  const q = $api.useInfiniteQuery(
    "get",
    "/api/v1/drafts",
    { params: { query: { state, limit: pageSize } } },
    { ...cursorPaging, staleTime: 5_000 },
  );
  return {
    items: q.data?.pages.flatMap((p) => p.drafts) ?? ([] as Draft[]),
    hasMore: q.hasNextPage,
    loadMore: () => void q.fetchNextPage(),
    isLoading: q.isLoading,
    isFetchingMore: q.isFetchingNextPage,
    error: q.error,
  };
}

export function useDraft(id: string | undefined) {
  return $api.useQuery("get", "/api/v1/drafts/{id}", { params: { path: { id: id ?? "" } } }, { enabled: !!id });
}

/** Outbox rows, newest first; they page by the last row's id. */
export function useOutbox(state: OutboxState[] | undefined, enabled = true) {
  const q = $api.useInfiniteQuery(
    "get",
    "/api/v1/outbox",
    { params: { query: { state, limit: 50 } } },
    { ...cursorPaging, staleTime: 5_000, enabled },
  );
  return {
    items: q.data?.pages.flatMap((p) => p.entries) ?? ([] as OutboxEntry[]),
    hasMore: q.hasNextPage,
    loadMore: () => void q.fetchNextPage(),
    isLoading: q.isLoading,
    isFetchingMore: q.isFetchingNextPage,
    error: q.error,
  };
}

/**
 * One outbox row. While it can still change by itself (queued, sending,
 * a retry pending) it is polled, so a result shows up without a reload.
 */
export function useOutboxEntry(id: string | undefined, enabled = true) {
  return $api.useQuery(
    "get",
    "/api/v1/outbox/{id}",
    { params: { path: { id: id ?? "" } } },
    {
      enabled: enabled && !!id,
      refetchInterval: (q) => (q.state.data && outboxMoves(q.state.data) ? 3_000 : false),
    },
  );
}

export function useThread(id: string | undefined) {
  return $api.useQuery(
    "get",
    "/api/v1/threads/{id}",
    { params: { path: { id: id ?? "" } } },
    { enabled: !!id, staleTime: 60_000 },
  );
}
