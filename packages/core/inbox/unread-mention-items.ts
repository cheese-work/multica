import type { InboxItem } from "../types";

function isUnreadMention(item: InboxItem) {
  return item.type === "mentioned" && !item.read && !item.archived;
}

/**
 * Issues holding a mention of the viewer they have not read. The server fans
 * direct mentions and `@all` out into `mentioned` inbox items, so the inbox
 * owns "read": opening the thread marks them read and the badge clears.
 */
export function unreadMentionIssueIds(items: readonly InboxItem[]): Set<string> {
  const ids = new Set<string>();
  for (const item of items) if (isUnreadMention(item) && item.issue_id) ids.add(item.issue_id);
  return ids;
}

/** Ids of the viewer's unread `mentioned` inbox items on one issue. */
export function unreadMentionItemIds(items: readonly InboxItem[], issueId: string): string[] {
  return items.filter((i) => isUnreadMention(i) && i.issue_id === issueId).map((i) => i.id);
}

const MAX_READ_FAILURES = 3;

/**
 * Guards "mark this mention read" requests for a screen that stays open: one
 * request per id at a time, none after the server accepted it, and a failed id
 * may be asked again, but only {@link MAX_READ_FAILURES} times so an offline
 * client does not loop.
 */
export type ReadRequestTracker = ReturnType<typeof createReadRequestTracker>;

export function createReadRequestTracker() {
  const inFlight = new Set<string>();
  const done = new Set<string>();
  const failures = new Map<string, number>();
  return {
    claim(id: string): boolean {
      if (inFlight.has(id) || done.has(id) || (failures.get(id) ?? 0) >= MAX_READ_FAILURES) return false;
      inFlight.add(id);
      return true;
    },
    settle(id: string, failed: boolean) {
      inFlight.delete(id);
      if (failed) failures.set(id, (failures.get(id) ?? 0) + 1);
      else done.add(id);
    },
  };
}

/**
 * Sends a read request for each of the issue's unread mentions the tracker
 * allows. `send` performs the write and calls `settle(failed)` when it ends.
 */
export function requestMentionReads(
  items: readonly InboxItem[],
  issueId: string,
  tracker: ReadRequestTracker,
  send: (id: string, settle: (failed: boolean) => void) => void,
) {
  for (const id of unreadMentionItemIds(items, issueId)) {
    if (tracker.claim(id)) send(id, (failed) => tracker.settle(id, failed));
  }
}

