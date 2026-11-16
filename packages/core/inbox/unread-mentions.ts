import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "../hooks";
import type { InboxItem } from "../types";
import { useMarkInboxRead } from "./mutations";
import { inboxListOptions } from "./queries";
import {
  createReadRequestTracker,
  requestMentionReads,
  unreadMentionIssueIds,
} from "./unread-mention-items";

export { unreadMentionIssueIds, unreadMentionItemIds } from "./unread-mention-items";

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
  const { mutate: markRead } = useMarkInboxRead();
  const [tracker] = useState(createReadRequestTracker);
  // A failed read rolls the cache back to the same unread items, which can
  // leave `data` unchanged; this bump re-runs the effect so the retry happens.
  const [failureCount, setFailureCount] = useState(0);

  useEffect(() => {
    if (!data || !issueId) return;
    requestMentionReads(data, issueId, tracker, (id, settle) =>
      markRead(id, {
        onSettled: (_result, error) => {
          settle(error !== null);
          if (error) setFailureCount((n) => n + 1);
        },
      }),
    );
  }, [data, issueId, markRead, tracker, failureCount]);
}
