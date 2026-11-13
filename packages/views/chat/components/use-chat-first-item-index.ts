import { useState } from "react";

/** Virtuoso needs room to count down as older rows are prepended. */
export const CHAT_FIRST_ITEM_INDEX_BASE = 1_000_000;

interface FirstItemIndexState {
  keys: string[];
  index: number;
}

function sameKeys(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((key, i) => key === b[i]);
}

function advance(state: FirstItemIndexState, keys: string[]): FirstItemIndexState {
  if (sameKeys(state.keys, keys)) return state;
  if (keys.length === 0 || state.keys.length === 0) {
    return { keys, index: CHAT_FIRST_ITEM_INDEX_BASE };
  }
  // A row that survives keeps its index: rows before it that are new were
  // prepended (index down), rows before it that are gone were dropped (index up).
  const position = new Map(keys.map((key, i) => [key, i]));
  for (let was = 0; was < state.keys.length; was += 1) {
    const now = position.get(state.keys[was]!);
    if (now !== undefined) return { keys, index: state.index + was - now };
  }
  return { keys, index: state.index };
}

/**
 * Virtuoso `firstItemIndex` for a chat list, from the rendered row keys
 * (oldest first). It follows the rows that really arrived or left at the
 * front, never page sizes: a refetch that only re-pages the same messages,
 * or slides the loaded window by one, leaves every surviving row at its index.
 */
export function useChatFirstItemIndex(keys: string[]): number {
  const [state, setState] = useState<FirstItemIndexState>({
    keys,
    index: CHAT_FIRST_ITEM_INDEX_BASE,
  });
  const next = advance(state, keys);
  if (next !== state) setState(next);
  return next.index;
}
