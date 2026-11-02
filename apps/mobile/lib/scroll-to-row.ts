/**
 * Scroll a virtualized list to a row and confirm it arrived (CHE-3116).
 *
 * `scrollToIndex` resolves after a fixed delay (FlashList 2.0.2: 200 ms
 * non-animated, 300 ms animated), not once the row is on screen. A row that was
 * never measured has an estimated offset, so a jump across many mixed-height
 * rows can settle short of it (a chip tap to an old comment landed among newer
 * filler rows). Check the visible range after each scroll and scroll again,
 * without animation, until the row shows or the attempts run out.
 *
 * The target is resolved again on every attempt: rows can be inserted or
 * removed above it while a scroll is in flight, so an index read earlier may
 * point at another row.
 */

export interface RowScroller {
  scrollToIndex(params: { index: number; animated: boolean }): Promise<void>;
  computeVisibleIndices(): { startIndex: number; endIndex: number };
}

const MAX_ATTEMPTS = 4;

/**
 * Resolves true when the target is in the visible range after a scroll.
 * `resolveIndex` returns the target's current row index, or -1 when it is gone.
 */
export async function scrollRowIntoView(
  list: RowScroller,
  resolveIndex: () => number,
  { isCancelled = () => false }: { isCancelled?: () => boolean } = {},
): Promise<boolean> {
  for (let attempt = 0; attempt < MAX_ATTEMPTS; attempt += 1) {
    const index = resolveIndex();
    if (index < 0) return false;
    await list.scrollToIndex({ index, animated: attempt === 0 });
    if (isCancelled()) return false;
    const current = resolveIndex();
    if (current < 0) return false;
    const { startIndex, endIndex } = list.computeVisibleIndices();
    if (current >= startIndex && current <= endIndex) return true;
  }
  return false;
}
