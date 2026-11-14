import { infiniteQueryOptions } from "@tanstack/react-query";
import { api } from "../api";
import { ApiError } from "../api/client";
import type { ChatSearchPage } from "../types/chat";
import { chatKeys } from "./queries";

/** Server limit for `GET /api/chat/search?q=` (characters, after trimming). */
export const CHAT_SEARCH_MAX_QUERY_LENGTH = 200;
const CHAT_SEARCH_PAGE_SIZE = 20;

export type ChatSearchFailure = "failed" | "timed_out" | "disconnected";

export function isChatSearchQueryTooLong(query: string): boolean {
  return query.trim().length > CHAT_SEARCH_MAX_QUERY_LENGTH;
}

/** The server answers 503 when the search outran its budget; a fetch that never got a response is a lost connection. */
export function chatSearchFailure(error: unknown): ChatSearchFailure {
  if (error instanceof ApiError) return error.status === 503 ? "timed_out" : "failed";
  return "disconnected";
}

export const chatSearchKeys = {
  query: (wsId: string, query: string) => [...chatKeys.all(wsId), "search", query] as const,
};

export function chatSearchInfiniteOptions(wsId: string, rawQuery: string) {
  const query = rawQuery.trim();
  return infiniteQueryOptions({
    queryKey: chatSearchKeys.query(wsId, query),
    queryFn: ({ pageParam }): Promise<ChatSearchPage> =>
      api.searchChats({ q: query, limit: CHAT_SEARCH_PAGE_SIZE, cursor: pageParam }),
    initialPageParam: null as string | null,
    getNextPageParam: (last: ChatSearchPage) => last.next_cursor ?? undefined,
    enabled: query.length > 0 && !isChatSearchQueryTooLong(query),
    staleTime: 30_000,
    retry: false,
  });
}
