import { queryOptions } from "@tanstack/react-query";
import type { InboxItem } from "@multica/core/types";
import { unreadMentionIssueIds } from "@multica/core/inbox/unread-mention-items";
import { api } from "@/data/api";

/**
 * Inbox cache key factory.
 *
 * Shape mirrors web's `packages/core/inbox/queries.ts` — `["inbox", wsId, "list"]`
 * — so cross-platform mental model stays the same. Keying on wsId means
 * workspace switches naturally invalidate (TQ sees a new key and refetches).
 */
export const inboxKeys = {
  all: (wsId: string | null) => ["inbox", wsId] as const,
  list: (wsId: string | null) =>
    [...inboxKeys.all(wsId), "list"] as const,
  // Account-level, not workspace-scoped: one cache entry holding unread
  // counts for every workspace the user belongs to. Same key shape as web
  // (packages/core/inbox/queries.ts) so the mental model stays shared.
  unreadSummary: () => ["inbox", "unread-summary"] as const,
};

export const inboxListOptions = (wsId: string | null) =>
  queryOptions({
    queryKey: inboxKeys.list(wsId),
    queryFn: ({ signal }) => api.listInbox({ signal }),
    enabled: !!wsId,
  });

/**
 * Cross-workspace unread inbox summary — the source of the tab badge count.
 *
 * Gated on an active workspace because the endpoint resolves through the
 * workspace-member middleware, same as web's sidebar does.
 */
export const inboxUnreadSummaryOptions = (wsId: string | null) =>
  queryOptions({
    queryKey: inboxKeys.unreadSummary(),
    queryFn: ({ signal }) => api.getInboxUnreadSummary({ signal }),
    enabled: !!wsId,
  });

/**
 * Whether one issue holds an unread mention of the viewer (direct or `@all`),
 * read off the inbox list the tab already keeps fresh. The selector is core's
 * pure one, so both clients agree.
 */
export const unreadMentionOptions = (wsId: string | null, issueId: string) => ({
  ...inboxListOptions(wsId),
  select: (items: InboxItem[]) => unreadMentionIssueIds(items).has(issueId),
});
