// @vitest-environment node
import { describe, expect, it } from "vitest";
import { isValidAutoCompactPct, readAutoCompact, resolveAutoCompact } from "./auto-compact";

// Retrospective test (added with the implementation, not first).
describe("readAutoCompact", () => {
  it("defaults to on at 80 when nothing is saved", () => {
    expect(readAutoCompact(undefined)).toEqual({ enabled: true, threshold_pct: 80 });
    expect(readAutoCompact({})).toEqual({ enabled: true, threshold_pct: 80 });
  });

  it("reads saved fields and defaults a missing one", () => {
    expect(readAutoCompact({ auto_compact: { enabled: false, threshold_pct: 60 } })).toEqual({
      enabled: false,
      threshold_pct: 60,
    });
    expect(readAutoCompact({ auto_compact: { threshold_pct: 70 } })).toEqual({
      enabled: true,
      threshold_pct: 70,
    });
  });

  it.each([
    ["a string", "on"],
    ["null", null],
    ["an array", []],
    ["a number", 5],
  ])("falls back to the defaults when auto_compact is %s", (_label, value) => {
    expect(readAutoCompact({ auto_compact: value })).toEqual({ enabled: true, threshold_pct: 80 });
  });

  it("falls back per field for malformed values", () => {
    expect(readAutoCompact({ auto_compact: { enabled: "yes", threshold_pct: "70" } })).toEqual({
      enabled: true,
      threshold_pct: 80,
    });
    expect(readAutoCompact({ auto_compact: { enabled: false, threshold_pct: 12 } })).toEqual({
      enabled: false,
      threshold_pct: 80,
    });
  });
});

describe("isValidAutoCompactPct", () => {
  it.each([50, 80, 90])("accepts %d", (value) => {
    expect(isValidAutoCompactPct(value)).toBe(true);
  });
  it.each([49, 91, 80.5, Number.NaN])("rejects %s", (value) => {
    expect(isValidAutoCompactPct(value)).toBe(false);
  });
});

// CHE-1808: the effective rule mirrors the server's precedence (agent override
// over workspace over the defaults, field by field).
describe("resolveAutoCompact", () => {
  it("defaults to on at 80", () => {
    expect(resolveAutoCompact(undefined, undefined)).toEqual({ enabled: true, threshold_pct: 80 });
  });

  it("lets a present agent field win over the workspace value", () => {
    expect(
      resolveAutoCompact(
        { auto_compact: { enabled: false, threshold_pct: 60 } },
        { auto_compact: { threshold_pct: 85 } },
      ),
    ).toEqual({ enabled: false, threshold_pct: 85 });
  });

  it("treats a null agent override as inherit and ignores malformed fields", () => {
    expect(resolveAutoCompact({ auto_compact: { threshold_pct: 55 } }, { auto_compact: null })).toEqual({
      enabled: true,
      threshold_pct: 55,
    });
    expect(resolveAutoCompact({ auto_compact: { threshold_pct: 55 } }, { auto_compact: { enabled: "no", threshold_pct: 5 } })).toEqual({
      enabled: true,
      threshold_pct: 55,
    });
  });
});
