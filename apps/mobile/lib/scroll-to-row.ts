/**
 * Scroll a virtualized list to a row and confirm it arrived (CHE-3116).
 *
 * `scrollToIndex` resolves once the scroll is issued, not once the row is on
 * screen. A row that was never measured has an estimated offset, so a jump
 * across many mixed-height rows can settle short of it (a chip tap to an old
 * comment landed among newer filler rows). Check the visible range after each
 * scroll and scroll again, without animation, until the row shows or the
 * attempts run out.
 */

export interface RowScroller {
  scrollToIndex(params: { index: number; animated: boolean }): Promise<void>;
  computeVisibleIndices(): { startIndex: number; endIndex: number };
}

const MAX_ATTEMPTS = 4;

/** Resolves true when `index` is in the visible range after a scroll. */
export async function scrollRowIntoView(
  list: RowScroller,
  index: number,
  { isCancelled = () => false }: { isCancelled?: () => boolean } = {},
): Promise<boolean> {
  for (let attempt = 0; attempt < MAX_ATTEMPTS; attempt += 1) {
    await list.scrollToIndex({ index, animated: attempt === 0 });
    if (isCancelled()) return false;
    const { startIndex, endIndex } = list.computeVisibleIndices();
    if (index >= startIndex && index <= endIndex) return true;
  }
  return false;
}
