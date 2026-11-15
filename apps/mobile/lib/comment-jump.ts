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
  /** Wait before the first scroll so a freshly mounted list can lay out. */
  delayMs?: number;
}

/**
 * Owns the one jump that may be running. A new jump or `cancel()` stops the
 * older one, including one still waiting out its `delayMs` (user drag,
 * unmount).
 */
export function createJumpController() {
  let token = 0;
  return {
    cancel() {
      token += 1;
    },
    /** `getList` is read after the delay; null means the list is gone. */
    async jump(
      getList: () => RowScroller | null,
      getRows: () => JumpRow[],
      commentId: string,
      { onSettled, holdMs = 0, delayMs = 0 }: JumpOptions = {},
    ): Promise<boolean> {
      token += 1;
      const mine = token;
      const cancelled = () => token !== mine;
      if (delayMs > 0) await new Promise((r) => setTimeout(r, delayMs));
      const list = cancelled() ? null : getList();
      const arrived = list
        ? await jumpToCommentRow(list, getRows, commentId, cancelled)
        : false;
      // A cancelled jump was superseded on purpose; only report a real miss.
      if (!arrived && !cancelled()) {
        console.warn(`[timeline] could not scroll to comment ${commentId}`);
      }
      if (onSettled) setTimeout(onSettled, holdMs);
      return arrived;
    },
  };
}
