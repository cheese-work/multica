import { useEffect, useRef, useState } from "react";
import {
  anchorFromViewable,
  saveReadingAnchor,
} from "@/lib/reading-anchor";

/**
 * FlashList viewability pair that records the reading anchor under `key`.
 * FlashList captures `viewabilityConfigCallbackPairs` at mount, so the pair is
 * built once and forwards to the latest key/ids through a ref. A null key (no
 * session yet) records nothing.
 */
export function useReadingAnchorRecorder(
  key: string | null,
  ids: string[],
  skipId?: string,
) {
  const latest = useRef({ key, ids, skipId });
  useEffect(() => {
    latest.current = { key, ids, skipId };
  });
  const [pair] = useState(() => ({
    viewabilityConfig: { itemVisiblePercentThreshold: 1 },
    onViewableItemsChanged: ({
      viewableItems,
    }: {
      viewableItems: { index: number | null }[];
    }) => {
      const { key, ids, skipId } = latest.current;
      if (!key) return;
      const anchor = anchorFromViewable(viewableItems, ids, skipId);
      if (anchor) saveReadingAnchor(key, anchor);
    },
  }));
  return pair;
}
