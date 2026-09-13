// @vitest-environment jsdom

import { afterEach, describe, expect, it, vi } from "vitest";
import { measureDescription } from "./description-measurement";

function rect(top: number, bottom: number): DOMRect {
  return { top, bottom, width: 10, height: bottom - top } as DOMRect;
}

function installRangeRects(
  getRects: (node: Text, end: number, selectedContents: boolean) => DOMRect[],
) {
  return vi.spyOn(document, "createRange").mockImplementation(() => {
    let node: Text | null = null;
    let end = 0;
    let selectedContents = false;
    return {
      selectNodeContents(next: Node) {
        node = next as Text;
        end = node.data.length;
        selectedContents = true;
      },
      setStart(next: Node) {
        node = next as Text;
        selectedContents = false;
      },
      setEnd(_next: Node, offset: number) {
        end = offset;
      },
      getClientRects() {
        return getRects(node!, end, selectedContents) as unknown as DOMRectList;
      },
    } as unknown as Range;
  });
}

function rootWithText(text: string): HTMLElement {
  const root = document.createElement("div");
  root.style.lineHeight = "10px";
  root.style.fontSize = "10px";
  root.append(text);
  vi.spyOn(root, "getBoundingClientRect").mockReturnValue(rect(100, 200));
  document.body.append(root);
  return root;
}

afterEach(() => {
  vi.restoreAllMocks();
  document.body.replaceChildren();
});

describe("measureDescription", () => {
  it("does not treat an empty bounded Range as visible text", () => {
    const root = rootWithText("hidden sentinel");
    installRangeRects((_node, _end, selectedContents) =>
      selectedContents ? [rect(100, 110)] : [],
    );

    const measurement = measureDescription(root);

    expect(measurement.totalRows).toBe(1);
    expect(measurement.previewText).toBe("");
  });

  it("excludes off-content text and image alt from preview and overflow", () => {
    const root = rootWithText("offscreen sentinel");
    const image = document.createElement("img");
    image.alt = "offscreen image sentinel";
    root.append(image);
    installRangeRects(() => [rect(0, 10)]);
    vi.spyOn(image, "getBoundingClientRect").mockReturnValue(rect(0, 10));

    const measurement = measureDescription(root);

    expect(measurement.totalRows).toBe(0);
    expect(measurement.hiddenRows).toBe(0);
    expect(measurement.hasOverflow).toBe(false);
    expect(measurement.previewText).toBe("");
  });
});
