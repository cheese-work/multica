/**
 * Reading-position anchor for the chat and issue-timeline lists. The list
 * remembers WHICH item sat at the top of the viewport, not a pixel offset:
 * rows are measured lazily, so a pixel offset restored into a fresh list lands
 * on different content once the rows above it settle. Mirrors web's
 * `packages/views/chat/components/chat-anchor.ts` (CHE-1857); the native list
 * restores to the item's top edge, so no in-item offset is kept.
 *
 * Memory only: lost on app restart, like web's view-state store.
 */

export interface ReadingAnchor {
  /** Top visible item. */
  itemId: string;
  /** The reader was at the newest item; reopen at the default position. */
  atLiveEnd: boolean;
}

export const chatAnchorKey = (sessionId: string) => `chat::${sessionId}`;
export const issueAnchorKey = (issueId: string) => `issue::${issueId}`;

const anchors = new Map<string, unknown>();

export function saveReadingAnchor(key: string, anchor: ReadingAnchor): void {
  anchors.set(key, anchor);
}

/** Pixel slack at the bottom edge that still counts as "at the live end". */
const LIVE_END_SLACK_PX = 80;

/**
 * The reader sits at the end of the content. Row visibility is not enough: a
 * tall last row counts as viewable while most of it is still below the fold.
 */
export function isNearEnd(
  contentHeight: number,
  offsetY: number,
  viewportHeight: number,
): boolean {
  return contentHeight - (offsetY + viewportHeight) < LIVE_END_SLACK_PX;
}

export function loadReadingAnchor(key: string): ReadingAnchor | null {
  return parseReadingAnchor(anchors.get(key));
}

/** Anything unreadable is "nothing saved": the default position is the safe fallback. */
export function parseReadingAnchor(value: unknown): ReadingAnchor | null {
  if (typeof value !== "object" || value === null) return null;
  const { itemId, atLiveEnd } = value as Record<string, unknown>;
  if (typeof itemId !== "string" || itemId === "") return null;
  if (typeof atLiveEnd !== "boolean") return null;
  return { itemId, atLiveEnd };
}

/**
 * Anchor from the currently viewable rows. `skipId` names a synthetic row
 * (the unread divider) that must never become the anchor. `nearEnd` is the
 * scroll-distance check (`isNearEnd`); the last row being viewable alone does
 * not make it the live end.
 */
export function anchorFromViewable(
  viewable: { index: number | null }[],
  ids: string[],
  skipId?: string,
  nearEnd = true,
): ReadingAnchor | null {
  const indexes = viewable
    .map((v) => v.index)
    .filter((i): i is number => i != null && i >= 0 && i < ids.length)
    .sort((a, b) => a - b);
  const top = indexes.find((i) => ids[i] !== skipId);
  if (top === undefined) return null;
  return {
    itemId: ids[top],
    atLiveEnd: nearEnd && indexes[indexes.length - 1] === ids.length - 1,
  };
}

/**
 * Index to open at, or undefined for the list's default (newest for chat,
 * top for an issue): no anchor, reader was at the live end, or the anchor
 * item is not loaded.
 */
export function restoreIndex(
  anchor: ReadingAnchor | null,
  ids: string[],
): number | undefined {
  if (!anchor || anchor.atLiveEnd) return undefined;
  const index = ids.indexOf(anchor.itemId);
  return index >= 0 ? index : undefined;
}

/**
 * Record the reader's position under `key`. No viewable row while rows exist
 * means the reader is above them (the issue header), so the stale anchor is
 * dropped and the next open starts at the top. An empty `ids` is a list that
 * is still loading: leave the saved anchor alone.
 */
export function recordReadingAnchor(
  key: string,
  viewable: { index: number | null }[],
  ids: string[],
  nearEnd: boolean,
  skipId?: string,
): void {
  if (ids.length === 0) return;
  const anchor = anchorFromViewable(viewable, ids, skipId, nearEnd);
  if (anchor) anchors.set(key, anchor);
  else anchors.delete(key);
}

/**
 * Per-list recording state. FlashList reports viewability ~250ms after a list
 * mounts, and the state outlives a session switch, so every cached value is
 * stamped with the key it was measured for. Until the current key has its own
 * viewability report, `record` does nothing: a saved anchor must survive a
 * scroll that lands before the first report, and one session's rows must never
 * be written under another's key.
 */
export function createAnchorTracker() {
  let viewKey: string | null = null;
  let viewable: { index: number | null }[] = [];
  let scrollKey: string | null = null;
  let nearEnd = true;
  return {
    onViewable(key: string, rows: { index: number | null }[]) {
      viewKey = key;
      viewable = rows;
    },
    onScroll(key: string, near: boolean) {
      scrollKey = key;
      nearEnd = near;
    },
    record(key: string, ids: string[], skipId?: string) {
      if (viewKey !== key) return;
      recordReadingAnchor(key, viewable, ids, scrollKey === key ? nearEnd : true, skipId);
    },
  };
}
