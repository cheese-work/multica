import { useState } from "react";

/** Virtuoso needs room to count down as older rows are prepended. */
export const CHAT_FIRST_ITEM_INDEX_BASE = 1_000_000;

interface FirstItemIndexState {
  oldestKey: string | null;
  index: number;
}

function advance(state: FirstItemIndexState, keys: string[]): FirstItemIndexState {
  const oldest = keys[0] ?? null;
  if (oldest === state.oldestKey) return state;
  if (oldest === null) return { oldestKey: null, index: CHAT_FIRST_ITEM_INDEX_BASE };
  if (state.oldestKey === null) return { oldestKey: oldest, index: CHAT_FIRST_ITEM_INDEX_BASE };
  // Rows in front of the previous oldest row were prepended. If it is gone
  // (removed, not prepended before), nothing shifted: keep the index.
  const prepended = Math.max(0, keys.indexOf(state.oldestKey));
  return { oldestKey: oldest, index: state.index - prepended };
}

/**
 * Virtuoso `firstItemIndex` for a chat list, from the rendered row keys
 * (oldest first). It moves only when rows really arrive before the previous
 * oldest row, never with page sizes, so a refetch that re-pages the same
 * messages does not shift the view.
 */
export function useChatFirstItemIndex(keys: string[]): number {
  const [state, setState] = useState<FirstItemIndexState>({
    oldestKey: keys[0] ?? null,
    index: CHAT_FIRST_ITEM_INDEX_BASE,
  });
  const next = advance(state, keys);
  if (next !== state) setState(next);
  return next.index;
}
