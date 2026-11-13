import { describe, expect, it } from "vitest";
import { mergeRestoredText } from "./merge-restored-text";

describe("mergeRestoredText", () => {
  it("restores the snapshot when nothing was typed meanwhile", () => {
    expect(mergeRestoredText("sent text", "")).toBe("sent text");
  });

  it("keeps text typed while the send was pending after the snapshot", () => {
    expect(mergeRestoredText("sent text", "typed later")).toBe(
      "sent text\ntyped later",
    );
  });

  it("does not duplicate identical text", () => {
    expect(mergeRestoredText("same", "same")).toBe("same");
  });
});
