import { describe, expect, it } from "vitest";
import { readScrollAnchor } from "./use-issue-detail-scroll-anchor";

function rect(top: number, bottom: number): DOMRect {
  return { top, bottom } as DOMRect;
}

function build(rows: Array<{ index: number; top: number; bottom: number }>) {
  const container = document.createElement("div");
  container.getBoundingClientRect = () => rect(100, 900);
  for (const r of rows) {
    const row = document.createElement("div");
    row.dataset.itemIndex = String(r.index);
    row.getBoundingClientRect = () => rect(r.top, r.bottom);
    container.appendChild(row);
  }
  return container;
}

const keyAt = (i: number) => `row-${i}`;

describe("readScrollAnchor", () => {
  it("anchors on the first row still visible, with its scrolled-out offset", () => {
    const container = build([
      { index: 4, top: -300, bottom: 100 },
      { index: 5, top: 60, bottom: 400 },
    ]);
    expect(readScrollAnchor(container, keyAt)).toEqual({ key: "row-5", offset: 40 });
  });

  it("returns null while the reader is above the first row", () => {
    const container = build([{ index: 0, top: 250, bottom: 500 }]);
    expect(readScrollAnchor(container, keyAt)).toBeNull();
  });

  it("returns null when the row has no key", () => {
    const container = build([{ index: 9, top: 100, bottom: 400 }]);
    expect(readScrollAnchor(container, () => undefined)).toBeNull();
  });
});
