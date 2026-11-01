// @vitest-environment node
import { describe, expect, it } from "vitest";
import { filterPresetCompletions } from "./presets";

const names = ["base", "reset", "fast", "deep", "Fable-xhigh"];

describe("filterPresetCompletions", () => {
  it("offers everything for an empty query, base and reset first", () => {
    expect(filterPresetCompletions(names, "")).toEqual(names);
  });
  it("filters as typed, case-insensitively, prefix first then substring", () => {
    expect(filterPresetCompletions(names, "f")).toEqual(["fast", "Fable-xhigh"]);
    expect(filterPresetCompletions(names, "ee")).toEqual(["deep"]);
    expect(filterPresetCompletions(names, "RES")).toEqual(["reset"]);
  });
  it("returns nothing when no name matches", () => {
    expect(filterPresetCompletions(names, "zzz")).toEqual([]);
  });
});
