// Reading-position anchor for a DM. The list remembers WHICH message sat at
// the top of the viewport, not a pixel offset: rows are measured lazily, so a
// pixel offset restored into a freshly mounted Virtuoso lands on different
// content as soon as the rows above it settle.

export interface ChatAnchor {
  /** Top visible message. */
  itemId: string;
  /** How far the viewport top sits below the message's own top edge. */
  offsetInItemPx: number;
  /** The reader was at the newest message; reopen there and keep following. */
  atLiveEnd: boolean;
}

/** View-state entry key. The route's pathname is added by the platform. */
export const chatAnchorKey = (sessionId: string) => `chat::${sessionId}`;

export function serializeChatAnchor(anchor: ChatAnchor): string {
  return JSON.stringify(anchor);
}

/** Anything unreadable is "nothing saved": opening at the newest is the safe default. */
export function parseChatAnchor(raw: string | undefined): ChatAnchor | null {
  if (!raw) return null;
  try {
    const value: unknown = JSON.parse(raw);
    if (typeof value !== "object" || value === null) return null;
    const { itemId, offsetInItemPx, atLiveEnd } = value as Record<string, unknown>;
    if (typeof itemId !== "string" || typeof atLiveEnd !== "boolean") return null;
    if (typeof offsetInItemPx !== "number" || !Number.isFinite(offsetInItemPx)) return null;
    return { itemId, offsetInItemPx: Math.max(0, offsetInItemPx), atLiveEnd };
  } catch {
    return null;
  }
}
