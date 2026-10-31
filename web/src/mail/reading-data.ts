import { useInfiniteQuery, useQuery } from "@tanstack/react-query";

import type { Facts, Content as ViewContent, Part as ViewPart, View as MessageView } from "@/api/agentview.gen";
import { getJSON } from "@/api/client";
import { keys } from "@/api/queries";
import type { Message } from "@/api/types.gen";

// The reading pane's data access, kept in this one module on purpose.
// The header and thread strip read the parsed fields of
// GET /api/v1/messages/{id}; the body and yuuka's facts come from the
// structured view (§8.1, §12.1) at .../view, the same tree get_message
// gives agents, so a person and an agent read the same paragraphs under
// the same anchors.

export function useMessage(id: string) {
  return useQuery({
    queryKey: keys.message(id),
    queryFn: () => getJSON<Message>(`/api/v1/messages/${encodeURIComponent(id)}`),
    // Derived content changes only on reparse; no need to refetch.
    staleTime: 5 * 60_000,
  });
}

/**
 * Where attachment section of message id downloads. The server always
 * answers with Content-Disposition: attachment; nothing from mail is
 * rendered in the page (§8.3).
 */
export function attachmentHref(id: string, section: string): string {
  return `/api/v1/messages/${encodeURIComponent(id)}/attachments/${encodeURIComponent(section)}`;
}

/** The structured view as far as it has been loaded. */
export interface Reading {
  facts: Facts;
  /** The header fields and attachments; the text is in parts. */
  content: ViewContent;
  parts: ViewPart[];
  /** The anchor the text continues at; undefined when it is all here. */
  more?: string;
}

/**
 * The structured view of message id, level part. The server sends at
 * most 32K characters at a time and says where to continue (`more`);
 * loadMore fetches the next stretch, which continues into later parts.
 */
export function useReading(id: string) {
  const q = useInfiniteQuery({
    queryKey: keys.reading(id),
    queryFn: ({ pageParam }) => {
      const p = new URLSearchParams({ level: "part" });
      if (pageParam) p.set("anchor", pageParam);
      return getJSON<MessageView>(`/api/v1/messages/${encodeURIComponent(id)}/view?${p}`);
    },
    initialPageParam: "",
    getNextPageParam: (last) => last.content.more || undefined,
    staleTime: 5 * 60_000,
  });
  return {
    reading: q.data?.pages[0] ? merge(q.data.pages[0], q.data.pages.slice(1)) : undefined,
    error: q.error,
    loadMore: () => void q.fetchNextPage(),
    isFetchingMore: q.isFetchingNextPage,
  };
}

/** Joins continuation pages; a part cut by the budget spans two of them. */
export function merge(first: MessageView, rest: MessageView[] = []): Reading {
  const pages = [first, ...rest];
  const parts: ViewPart[] = [];
  for (const page of pages) {
    for (const p of page.content.parts ?? []) {
      const last = parts.at(-1);
      if (last?.anchor === p.anchor) parts[parts.length - 1] = { ...last, blocks: [...last.blocks, ...p.blocks] };
      else parts.push(p);
    }
  }
  return {
    facts: first.facts,
    content: { ...first.content, parts: undefined, more: undefined },
    parts,
    more: pages.at(-1)?.content.more || undefined,
  };
}
