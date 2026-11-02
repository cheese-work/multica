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
