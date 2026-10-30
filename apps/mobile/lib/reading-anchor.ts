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
 * (the unread divider) that must never become the anchor.
 */
export function anchorFromViewable(
  viewable: { index: number | null }[],
  ids: string[],
  skipId?: string,
): ReadingAnchor | null {
  const indexes = viewable
    .map((v) => v.index)
    .filter((i): i is number => i != null && i >= 0 && i < ids.length)
    .sort((a, b) => a - b);
  const top = indexes.find((i) => ids[i] !== skipId);
  if (top === undefined) return null;
  return {
    itemId: ids[top],
    atLiveEnd: indexes[indexes.length - 1] === ids.length - 1,
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
