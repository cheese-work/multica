/**
 * Scroll a virtualized list to a row and confirm it arrived (CHE-3116).
 *
 * `scrollToIndex` resolves after a fixed delay (FlashList 2.0.2: 200 ms
 * non-animated, 300 ms animated), not once the row is on screen. A row that was
 * never measured has an estimated offset, so a jump across many mixed-height
 * rows can settle short of it (a chip tap to an old comment landed among newer
 * filler rows). Check the visible range after each scroll and scroll again
 * while each pass still moves it.
 *
 * Re-aiming at the target can reach a fixed point: the rows above the visible
 * range are never measured, so their estimate error never shrinks. Then walk
 * down from the top instead. The rows above each step are on screen one step
 * earlier, so every estimate along the walk is exact.
 *
 * The target is resolved again on every attempt: rows can be inserted or
 * removed above it while a scroll is in flight, so an index read earlier may
 * point at another row.
 */

export interface RowScroller {
  scrollToIndex(params: { index: number; animated: boolean }): Promise<void>;
  computeVisibleIndices(): { startIndex: number; endIndex: number };
}

// Bounds the total scrolls of one jump, both phases together.
const MAX_ATTEMPTS = 64;

/**
 * Resolves true when the target is in the visible range after a scroll.
 * `resolveIndex` returns the target's current row index, or -1 when it is gone.
 * False means the row is gone, the jump was cancelled, or the list stopped
 * moving short of it; the caller decides how to report that.
 */
export async function scrollRowIntoView(
  list: RowScroller,
  resolveIndex: () => number,
  { isCancelled = () => false }: { isCancelled?: () => boolean } = {},
): Promise<boolean> {
  let attempts = 0;
  // One scroll, then: true = arrived, false = give up, null = keep going.
  const step = async (
    pick: (target: number) => number,
    animated: boolean,
  ): Promise<{ done: boolean | null; range: string }> => {
    attempts += 1;
    const target = resolveIndex();
    if (target < 0) return { done: false, range: "" };
    await list.scrollToIndex({ index: pick(target), animated });
    if (isCancelled()) return { done: false, range: "" };
    const current = resolveIndex();
    if (current < 0) return { done: false, range: "" };
    const { startIndex, endIndex } = list.computeVisibleIndices();
    if (current >= startIndex && current <= endIndex) {
      return { done: true, range: "" };
    }
    return { done: null, range: `${startIndex}:${endIndex}` };
  };

  // Phase 1: re-aim at the target until the range stops moving.
  let lastRange = "";
  while (attempts < MAX_ATTEMPTS) {
    const { done, range } = await step((t) => t, attempts === 0);
    if (done !== null) return done;
    if (range === lastRange) break;
    lastRange = range;
  }

  // Phase 2: walk down from the top, one visible range at a time.
  let next = 0;
  lastRange = "";
  while (attempts < MAX_ATTEMPTS) {
    const { done, range } = await step((t) => Math.min(next, t), false);
    if (done !== null) return done;
    if (range === lastRange) return false;
    lastRange = range;
    next = Number(range.split(":")[1]) + 1;
  }
  return false;
}
