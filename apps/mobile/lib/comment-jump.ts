import { scrollRowIntoView, type RowScroller } from "./scroll-to-row";

interface JumpRow {
  entry: { id: string };
  replies: { id: string }[];
}

/**
 * Index of the row that holds a comment: its own row for a root, the parent's
 * row for a reply (replies render inline inside their root's card).
 */
export function rowIndexOfComment(rows: JumpRow[], commentId: string): number {
  return rows.findIndex(
    (row) =>
      row.entry.id === commentId ||
      row.replies.some((reply) => reply.id === commentId),
  );
}

/**
 * Scroll the timeline to a comment and confirm the row is on screen. `getRows`
 * reads the latest rows on every attempt, so rows added or removed while the
 * scroll runs do not leave the jump aimed at a stale index.
 */
export function jumpToCommentRow(
  list: RowScroller,
  getRows: () => JumpRow[],
  commentId: string,
  isCancelled: () => boolean,
): Promise<boolean> {
  return scrollRowIntoView(
    list,
    () => rowIndexOfComment(getRows(), commentId),
    { isCancelled },
  );
}
