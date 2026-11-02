import { describe, expect, it } from "vitest";
import { jumpToCommentRow, rowIndexOfComment } from "./comment-jump";
import type { RowScroller } from "./scroll-to-row";

const ESTIMATE = 100; // fixed guess for an unmeasured row
const VIEWPORT = 600;

interface Row {
  entry: { id: string };
  replies: { id: string }[];
}

/**
 * Host model of a lazily measured list (not FlashList): unmeasured rows count
 * as ESTIMATE tall when a scroll offset is computed, but the viewport shows the
 * real layout, and rows that were on screen become measured. Starts at the
 * bottom with only the last viewport measured, like a freshly remounted
 * timeline after a chip tap.
 */
function lazyList(heights: number[]) {
  const measured = new Set<number>();
  const actualTop = (i: number) => heights.slice(0, i).reduce((a, b) => a + b, 0);
  const estimatedTop = (i: number) =>
    heights.slice(0, i).reduce((a, h, k) => a + (measured.has(k) ? h : ESTIMATE), 0);
  const visibleAt = (offset: number) => {
    const rows: number[] = [];
    heights.forEach((h, i) => {
      if (actualTop(i) + h > offset && actualTop(i) < offset + VIEWPORT) rows.push(i);
    });
    return { startIndex: rows[0] ?? 0, endIndex: rows[rows.length - 1] ?? 0 };
  };
  let offset = actualTop(heights.length) - VIEWPORT;
  const v = visibleAt(offset);
  for (let i = v.startIndex; i <= v.endIndex; i += 1) measured.add(i);
  const calls: number[] = [];
  const list: RowScroller = {
    async scrollToIndex({ index }) {
      calls.push(index);
      offset = estimatedTop(index);
      const seen = visibleAt(offset);
      for (let i = seen.startIndex; i <= seen.endIndex; i += 1) measured.add(i);
    },
    computeVisibleIndices: () => visibleAt(offset),
  };
  return { list, calls };
}

// Old root + reply, then 35 newer filler comments with varied heights (M2-1).
const OLD_ROOT = "old-root";
const REPLY = "old-reply";
function timeline(): Row[] {
  return [
    { entry: { id: OLD_ROOT }, replies: [{ id: REPLY }] },
    ...Array.from({ length: 35 }, (_, i) => ({
      entry: { id: `filler-${i + 1}` },
      replies: [],
    })),
  ];
}
const HEIGHTS = Array.from({ length: 36 }, (_, i) => 100 + ((i * 37) % 5) * 40);
const TARGET_ROW = 12; // an old root that sits well above the bottom
function timelineWithTargetAt(row: number): Row[] {
  const rows = timeline().slice(1);
  rows.splice(row, 0, { entry: { id: OLD_ROOT }, replies: [{ id: REPLY }] });
  return rows;
}

describe("rowIndexOfComment", () => {
  it("finds a root by its own id and a reply by its parent's row", () => {
    const rows = timeline();
    expect(rowIndexOfComment(rows, OLD_ROOT)).toBe(0);
    expect(rowIndexOfComment(rows, REPLY)).toBe(0);
    expect(rowIndexOfComment(rows, "filler-3")).toBe(3);
    expect(rowIndexOfComment(rows, "missing")).toBe(-1);
  });
});

describe("jumpToCommentRow", () => {
  it("lands on the referenced row on every repeated tap", async () => {
    for (let tap = 1; tap <= 3; tap += 1) {
      // Each chip tap pushes a fresh screen; going back and tapping again
      // starts another fresh list at the bottom.
      const rows = timelineWithTargetAt(TARGET_ROW);
      const { list } = lazyList(HEIGHTS);
      const arrived = await jumpToCommentRow(list, () => rows, OLD_ROOT, () => false);
      const { startIndex, endIndex } = list.computeVisibleIndices();
      expect(arrived, `tap ${tap}`).toBe(true);
      expect(TARGET_ROW, `tap ${tap}`).toBeGreaterThanOrEqual(startIndex);
      expect(TARGET_ROW, `tap ${tap}`).toBeLessThanOrEqual(endIndex);
    }
  });

  it("keeps scrolling while each pass makes progress (36 rows of 260, estimate 100)", async () => {
    // CHE-3116 review: four fixed passes only reached rows 8-10 of a target at row 12.
    const rows = timelineWithTargetAt(TARGET_ROW);
    const { list, calls } = lazyList(Array.from({ length: 36 }, () => 260));
    const arrived = await jumpToCommentRow(list, () => rows, OLD_ROOT, () => false);
    const { startIndex, endIndex } = list.computeVisibleIndices();
    expect(calls.length).toBeGreaterThan(4);
    expect(arrived).toBe(true);
    expect(TARGET_ROW).toBeGreaterThanOrEqual(startIndex);
    expect(TARGET_ROW).toBeLessThanOrEqual(endIndex);
  });

  it("scrolls to a reply's parent row", async () => {
    const rows = timelineWithTargetAt(TARGET_ROW);
    const { list } = lazyList(HEIGHTS);
    expect(await jumpToCommentRow(list, () => rows, REPLY, () => false)).toBe(true);
    const { startIndex, endIndex } = list.computeVisibleIndices();
    expect(TARGET_ROW).toBeGreaterThanOrEqual(startIndex);
    expect(TARGET_ROW).toBeLessThanOrEqual(endIndex);
  });

  it("follows the row when newer rows are inserted above it mid-scroll", async () => {
    const INSERTED = 5;
    let rows = timelineWithTargetAt(TARGET_ROW);
    // Layout after the insertion: INSERTED new rows sit above the old ones.
    const { list, calls } = lazyList([
      ...Array.from({ length: INSERTED }, () => 150),
      ...HEIGHTS,
    ]);
    const inserting: RowScroller = {
      ...list,
      async scrollToIndex(p) {
        await list.scrollToIndex(p);
        if (calls.length === 1) {
          // Comments land above the target; its row index moves down.
          rows = [
            ...Array.from({ length: INSERTED }, (_, i) => ({
              entry: { id: `new-${i}` },
              replies: [],
            })),
            ...rows,
          ];
        }
      },
    };
    const arrived = await jumpToCommentRow(inserting, () => rows, OLD_ROOT, () => false);
    const now = rowIndexOfComment(rows, OLD_ROOT);
    const { startIndex, endIndex } = list.computeVisibleIndices();
    expect(now).toBe(TARGET_ROW + INSERTED);
    expect(arrived).toBe(true);
    expect(calls.at(-1)).toBe(now);
    expect(now).toBeGreaterThanOrEqual(startIndex);
    expect(now).toBeLessThanOrEqual(endIndex);
  });
});
