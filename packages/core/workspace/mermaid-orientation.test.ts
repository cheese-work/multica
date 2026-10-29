import { describe, expect, it } from "vitest";
import { readMermaidOrientationSetting } from "./mermaid-orientation";

describe("readMermaidOrientationSetting", () => {
  it("reads author", () => {
    expect(readMermaidOrientationSetting({ mermaid_orientation: "author" })).toBe("author");
  });

  it.each([undefined, {}, { mermaid_orientation: "sideways" }, { mermaid_orientation: 3 }, { mermaid_orientation: null }])(
    "falls back to auto for %j",
    (settings) => {
      expect(readMermaidOrientationSetting(settings)).toBe("auto");
    },
  );
});
