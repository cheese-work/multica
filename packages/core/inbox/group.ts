import type { InboxItem } from "../types";

function byNewest(a: InboxItem, b: InboxItem): number {
  return new Date(b.created_at).getTime() - new Date(a.created_at).getTime();
}

/** Notifications grouped by issue (`issue_id ?? id`), each group newest first. */
export function groupInboxRows(items: readonly InboxItem[]): InboxItem[][] {
  const groups = new Map<string, InboxItem[]>();
  for (const item of items) {
    const key = item.issue_id ?? item.id;
    const group = groups.get(key);
    if (group) group.push(item);
    else groups.set(key, [item]);
  }
  const out = [...groups.values()];
  for (const group of out) group.sort(byNewest);
  return out;
}

/**
 * True when the viewer was named directly or by `@all`. The server writes
 * `mentioned` for `@squad` too and records which source it was in
 * `details.mention_source`. A row from before that was recorded has no source,
 * so its origin is unknown and it does not match.
 */
export function isViewerMention(item: InboxItem): boolean {
  const source = item.details?.mention_source;
  return source === "direct" || source === "all";
}

/**
 * Copies the group's viewer mention onto the row it renders as, so a later
 * comment cannot hide a mention from "Mentions me" on a deduplicated list
 * (mobile filters that list directly).
 */
export function withMentionSource(row: InboxItem, group: readonly InboxItem[]): InboxItem {
  if (isViewerMention(row)) return row;
  const mention = group.find(isViewerMention);
  if (!mention) return row;
  return { ...row, details: { ...(row.details ?? {}), ...(mention.details?.mention_source ? { mention_source: mention.details.mention_source } : {}) } };
}

/**
 * The row a group renders as. It keeps the group's newest comment anchor when
 * it has none of its own, so opening it still scrolls to the comment.
 */
export function withCommentAnchor(row: InboxItem, group: readonly InboxItem[]): InboxItem {
  if (row.details?.comment_id) return row;
  const commentId = group.find((item) => item.details?.comment_id)?.details?.comment_id;
  if (!commentId) return row;
  return { ...row, details: { ...(row.details ?? {}), comment_id: commentId } };
}

/** One row per issue: its newest notification. Newest group first. */
export function groupInboxItemsByIssue(items: readonly InboxItem[]): InboxItem[] {
  return groupInboxRows(items)
    .flatMap((group) => (group[0] ? [withMentionSource(withCommentAnchor(group[0], group), group)] : []))
    .sort(byNewest);
}

export function sortInboxNewestFirst(items: InboxItem[]): InboxItem[] {
  return items.sort(byNewest);
}
