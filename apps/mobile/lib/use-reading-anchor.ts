import { useEffect, useRef, useState } from "react";
import type { NativeScrollEvent, NativeSyntheticEvent } from "react-native";
import { isNearEnd, recordReadingAnchor } from "@/lib/reading-anchor";

type Viewable = { index: number | null }[];

/**
 * Records the reading anchor under `key` from FlashList viewability plus
 * scroll distance. FlashList captures `viewabilityConfigCallbackPairs` at
 * mount, so the pair is built once and forwards to the latest key/ids through
 * a ref. A null key (no session yet) records nothing. Pass `onScroll` to the
 * list too: it keeps the live-end flag in step with a tall last row that
 * never changes the viewable set.
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
  const viewable = useRef<Viewable>([]);
  const nearEnd = useRef(true);

  const record = () => {
    const { key, ids, skipId } = latest.current;
    if (key) recordReadingAnchor(key, viewable.current, ids, nearEnd.current, skipId);
  };

  const [pair] = useState(() => ({
    viewabilityConfig: { itemVisiblePercentThreshold: 1 },
    onViewableItemsChanged: ({ viewableItems }: { viewableItems: Viewable }) => {
      viewable.current = viewableItems;
      record();
    },
  }));
  const [onScroll] = useState(
    () => (e: NativeSyntheticEvent<NativeScrollEvent>) => {
      const { contentOffset, contentSize, layoutMeasurement } = e.nativeEvent;
      nearEnd.current = isNearEnd(
        contentSize.height,
        contentOffset.y,
        layoutMeasurement.height,
      );
      record();
    },
  );
  return { pair, onScroll };
}
