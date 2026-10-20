// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "../api/client";
import { AutoCompactSchema, isValidAutoCompactPct, readAutoCompact, resolveAutoCompact } from "./auto-compact";

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

// Retrospective tests (added after the implementation, to close the
// malformed-response gap in review): the schema, and the read of a whole
// workspace response whose settings blob is malformed.
describe("AutoCompactSchema", () => {
  it.each([
    ["undefined", undefined],
    ["null", null],
    ["a string", "on"],
    ["an array", [1, 2]],
    ["a number", 7],
    ["an empty object", {}],
  ])("returns the defaults for %s", (_label, value) => {
    expect(AutoCompactSchema.parse(value)).toEqual({ enabled: true, threshold_pct: 80 });
  });

  it.each([
    ["NaN", Number.NaN],
    ["Infinity", Number.POSITIVE_INFINITY],
    ["below the range", 49],
    ["above the range", 91],
    ["a fraction", 80.5],
    ["a string", "70"],
    ["null", null],
    ["a boolean", true],
  ])("replaces a threshold_pct that is %s with 80 and keeps a valid enabled", (_label, threshold) => {
    expect(AutoCompactSchema.parse({ enabled: false, threshold_pct: threshold })).toEqual({
      enabled: false,
      threshold_pct: 80,
    });
  });

  it.each([
    ["a string", "false"],
    ["a number", 0],
    ["null", null],
  ])("replaces an enabled that is %s with true and keeps a valid threshold_pct", (_label, enabled) => {
    expect(AutoCompactSchema.parse({ enabled, threshold_pct: 60 })).toEqual({
      enabled: true,
      threshold_pct: 60,
    });
  });

  it("drops unknown keys", () => {
    expect(AutoCompactSchema.parse({ enabled: false, threshold_pct: 55, token_cap: 1000 })).toEqual({
      enabled: false,
      threshold_pct: 55,
    });
  });
});

describe("readAutoCompact on a malformed workspace response", () => {
  afterEach(() => vi.unstubAllGlobals());

  async function readFromResponse(body: unknown) {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }),
      ),
    );
    const workspace = await new ApiClient("https://api.example.test").getWorkspace("ws-1");
    return readAutoCompact(workspace.settings);
  }

  it("keeps the saved values from a well-formed response", async () => {
    expect(
      await readFromResponse({ id: "ws-1", settings: { auto_compact: { enabled: false, threshold_pct: 65 } } }),
    ).toEqual({ enabled: false, threshold_pct: 65 });
  });

  it.each([
    ["settings is missing", { id: "ws-1" }],
    ["settings is null", { id: "ws-1", settings: null }],
    ["settings is a string", { id: "ws-1", settings: "x" }],
    ["auto_compact is a string", { id: "ws-1", settings: { auto_compact: "off" } }],
    ["auto_compact fields are garbage", { id: "ws-1", settings: { auto_compact: { enabled: [], threshold_pct: {} } } }],
  ])("returns the defaults when %s", async (_label, body) => {
    expect(await readFromResponse(body)).toEqual({ enabled: true, threshold_pct: 80 });
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
