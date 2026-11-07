import { describe, expect, it } from "vitest";
import { questionLines } from "./open-asks";

describe("questionLines", () => {
  it("scans a huge line of trailing underscores in linear time", () => {
    const started = performance.now();
    const asks = questionLines(`Which database should we use?${"_".repeat(60_000)}x\nWhich one?${"_".repeat(60_000)}`);
    expect(performance.now() - started).toBeLessThan(200);
    expect(asks).toEqual([]); // lines over 1000 characters are prose, not asks
  });

  it("strips bullets and emphasis markers, skips fences", () => {
    expect(questionLines("- **Which database?**\n```\nwhat?\n```\n2) Why?")).toEqual(["**Which database?", "Why?"]);
  });
});
