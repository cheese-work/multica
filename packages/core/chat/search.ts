import { infiniteQueryOptions } from "@tanstack/react-query";
import { create } from "zustand";
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

export type ChatSearchTextPart = { text: string; match: boolean };

/** Splits `text` around the first case-insensitive occurrence of `query`; the match is plain text, never a pattern. */
export function splitHighlight(text: string, rawQuery: string): ChatSearchTextPart[] {
  const query = rawQuery.trim();
  const index = query ? text.toLowerCase().indexOf(query.toLowerCase()) : -1;
  if (index < 0) return [{ text, match: false }];
  const end = index + query.length;
  return [
    { text: text.slice(0, index), match: false },
    { text: text.slice(index, end), match: true },
    { text: text.slice(end), match: false },
  ].filter((part) => part.text.length > 0);
}

/** Where a search field lives: the full page rail or the floating window popover. */
export type ChatSearchScope = "page" | "popover";

/**
 * The query sits outside the field's component so it survives the list
 * remounting and the history Back that returns to it: the rail keeps the query
 * and the cached results, and Escape or Clear is the only way to drop them.
 */
export const useChatSearchStore = create<{
  queries: Partial<Record<ChatSearchScope, string>>;
  setQuery: (scope: ChatSearchScope, query: string) => void;
  clear: (scope: ChatSearchScope) => void;
}>((set) => ({
  queries: {},
  setQuery: (scope, query) => set((s) => ({ queries: { ...s.queries, [scope]: query } })),
  clear: (scope) =>
    set((s) => {
      const { [scope]: _dropped, ...rest } = s.queries;
      return { queries: rest };
    }),
}));
