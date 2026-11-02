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

interface JumpOptions {
  /**
   * Called `holdMs` after the jump settles (arrived, gave up or cancelled), so a
   * long jump keeps its highlight for the full hold once the row is on screen.
   */
  onSettled?: () => void;
  holdMs?: number;
}

/**
 * Owns the one jump that may be running. A new jump or `cancel()` stops the
 * older one's retries (user drag, unmount).
 */
export function createJumpController() {
  let token = 0;
  return {
    cancel() {
      token += 1;
    },
    async jump(
      list: RowScroller,
      getRows: () => JumpRow[],
      commentId: string,
      { onSettled, holdMs = 0 }: JumpOptions = {},
    ): Promise<boolean> {
      token += 1;
      const mine = token;
      const arrived = await jumpToCommentRow(list, getRows, commentId, () => token !== mine);
      // A cancelled jump was superseded on purpose; only report a real miss.
      if (!arrived && token === mine) {
        console.warn(`[timeline] could not scroll to comment ${commentId}`);
      }
      if (onSettled) setTimeout(onSettled, holdMs);
      return arrived;
    },
  };
}
