import { useEffect, useRef } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "../hooks";
import type { InboxItem } from "../types";
import { useMarkInboxRead } from "./mutations";
import { inboxListOptions } from "./queries";
import { unreadMentionIssueIds, unreadMentionItemIds } from "./unread-mention-items";

export { unreadMentionIssueIds, unreadMentionItemIds };

/** Rides the inbox list the app already prefetches; rows add no request. */
export function useHasUnreadMention(wsId: string, issueId: string): boolean {
  const { data } = useQuery({
    ...inboxListOptions(wsId),
    select: (items: InboxItem[]) => unreadMentionIssueIds(items).has(issueId),
  });
  return data === true;
}

/**
 * Opening an issue (its thread, or a comment deep link) reads the viewer's
 * mentions on it. The inbox page does the same on select; this covers every
 * other way in, so the list badge clears.
 */
export function useMarkMentionsReadOnOpen(issueId: string | null | undefined) {
  const wsId = useWorkspaceId();
  const { data } = useQuery(inboxListOptions(wsId));
  const markRead = useMarkInboxRead();
  const requested = useRef(new Set<string>());

  useEffect(() => {
    if (!data || !issueId) return;
    for (const id of unreadMentionItemIds(data, issueId)) {
      if (requested.current.has(id)) continue;
      requested.current.add(id);
      markRead.mutate(id);
    }
  }, [data, issueId, markRead.mutate]);
}
