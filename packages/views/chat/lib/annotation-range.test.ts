// @vitest-environment node
import { describe, expect, it } from "vitest";
import { visibleRowRange, type RowBox } from "./annotation-range";

// 20 rows, 56 px high with an 8 px gap, in a 272 px viewport (rev-2 frame 5 and 6).
const rows: RowBox[] = Array.from({ length: 20 }, (_, i) => ({ top: i * 64, height: 56 }));
const total = 20 * 64 - 8;

describe("visibleRowRange", () => {
  it("counts only rows that fit whole at the top", () => {
    expect(visibleRowRange(rows, 0, 272)).toEqual({ from: 1, to: 4 });
  });

  it("reaches the last row at the end of the scroll", () => {
    expect(visibleRowRange(rows, total - 272, 272)).toEqual({ from: 17, to: 20 });
  });

  it("falls back to the row in view when no row fits whole", () => {
    expect(visibleRowRange(rows, 10, 40)).toEqual({ from: 1, to: 1 });
    expect(visibleRowRange(rows, 70, 40)).toEqual({ from: 2, to: 2 });
  });

  it("has no range for an empty list or a zero-height viewport", () => {
    expect(visibleRowRange([], 0, 272)).toBeNull();
    expect(visibleRowRange(rows, 0, 0)).toBeNull();
  });
});
