import { describe, expect, it } from "vitest";
import { scrollRowIntoView, type RowScroller } from "./scroll-to-row";

/** Fake list whose first `shortBy` scrolls land `drift` rows short of the target,
 *  like FlashList landing on estimated offsets before rows above are measured. */
function fakeList(opts: { shortBy: number; drift?: number; viewport?: number }) {
  const { shortBy, drift = 20, viewport = 5 } = opts;
  let top = 100;
  let calls = 0;
  const animatedFlags: boolean[] = [];
  const list: RowScroller = {
    async scrollToIndex({ index, animated }) {
      animatedFlags.push(animated);
      calls += 1;
      top = calls <= shortBy ? index + drift * (shortBy - calls + 1) : index;
    },
    computeVisibleIndices: () => ({ startIndex: top, endIndex: top + viewport - 1 }),
  };
  return { list, calls: () => calls, animatedFlags };
}

describe("scrollRowIntoView", () => {
  it("returns true after one scroll when the row is already visible", async () => {
    const f = fakeList({ shortBy: 0 });
    expect(await scrollRowIntoView(f.list, () => 0)).toBe(true);
    expect(f.calls()).toBe(1);
  });

  it("scrolls again when the first scroll lands short of the row", async () => {
    const f = fakeList({ shortBy: 1 });
    expect(await scrollRowIntoView(f.list, () => 0)).toBe(true);
    expect(f.calls()).toBe(2);
    expect(f.list.computeVisibleIndices().startIndex).toBe(0);
  });

  it("retries without animation after the first animated attempt", async () => {
    const f = fakeList({ shortBy: 2 });
    await scrollRowIntoView(f.list, () => 0);
    expect(f.animatedFlags).toEqual([true, false, false]);
  });

  it("gives up and reports false when the list never moves", async () => {
    let calls = 0;
    const list: RowScroller = {
      async scrollToIndex() {
        calls += 1;
      },
      computeVisibleIndices: () => ({ startIndex: 100, endIndex: 104 }),
    };
    expect(await scrollRowIntoView(list, () => 0)).toBe(false);
    // Two re-aims see the same range, then two walk steps see it again.
    expect(calls).toBe(4);
  });

  it("walks down from the top when re-aiming at the target stalls", async () => {
    // Re-aiming at row 18 always lands on rows 5-9; a scroll to a row above it
    // lands there, so stepping down from row 0 reaches the target.
    let top = 5;
    const asked: number[] = [];
    const list: RowScroller = {
      async scrollToIndex({ index }) {
        asked.push(index);
        top = index === 18 ? 5 : index;
      },
      computeVisibleIndices: () => ({ startIndex: top, endIndex: top + 4 }),
    };
    expect(await scrollRowIntoView(list, () => 18)).toBe(true);
    expect(asked).toEqual([18, 18, 0, 5, 10, 15]);
    expect(list.computeVisibleIndices().startIndex).toBe(15);
  });

  it("stops at the attempt cap when the range keeps drifting", async () => {
    let top = 100;
    let calls = 0;
    const list: RowScroller = {
      async scrollToIndex() {
        calls += 1;
        top += 1; // always moves, never arrives
      },
      computeVisibleIndices: () => ({ startIndex: top, endIndex: top + 4 }),
    };
    expect(await scrollRowIntoView(list, () => 0)).toBe(false);
    expect(calls).toBe(64);
  });

  it("stops retrying once cancelled", async () => {
    const f = fakeList({ shortBy: 99 });
    let cancelled = false;
    const list: RowScroller = {
      ...f.list,
      async scrollToIndex(p) {
        await f.list.scrollToIndex(p);
        cancelled = true;
      },
    };
    expect(await scrollRowIntoView(list, () => 0, { isCancelled: () => cancelled })).toBe(false);
    expect(f.calls()).toBe(1);
  });

  it("aims every attempt at the row's current index when rows shift in flight", async () => {
    const f = fakeList({ shortBy: 1 });
    let target = 5;
    const list: RowScroller = {
      ...f.list,
      async scrollToIndex(p) {
        await f.list.scrollToIndex(p);
        target = 7; // two rows were inserted above the target mid-scroll
      },
    };
    const asked: number[] = [];
    const spy: RowScroller = {
      ...list,
      scrollToIndex: (p) => {
        asked.push(p.index);
        return list.scrollToIndex(p);
      },
    };
    expect(await scrollRowIntoView(spy, () => target)).toBe(true);
    expect(asked).toEqual([5, 7]);
  });

  it("returns false when the target row disappears", async () => {
    const f = fakeList({ shortBy: 0 });
    expect(await scrollRowIntoView(f.list, () => -1)).toBe(false);
    expect(f.calls()).toBe(0);
  });
});
