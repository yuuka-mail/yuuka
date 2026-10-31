import { useQuery } from "@tanstack/react-query";

import { getJSON } from "@/api/client";
import { keys } from "@/api/queries";
import type { Message } from "@/api/types.gen";

// The reading pane's data access, kept in this one module on purpose.
// M1 reads the parsed fields and plain body_text of GET
// /api/v1/messages/{id}; the structured agent view (§8.1, §12.1) replaces
// the body source here and the renderer in reading-body.tsx, and nothing
// else in the reading pane has to change.

export function useMessage(id: string) {
  return useQuery({
    queryKey: keys.message(id),
    queryFn: () => getJSON<Message>(`/api/v1/messages/${encodeURIComponent(id)}`),
    // Derived content changes only on reparse; no need to refetch.
    staleTime: 5 * 60_000,
  });
}
