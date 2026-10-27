// @vitest-environment node
import { describe, expect, it } from "vitest";
import { diffSettings, formatConfidence, restoresRoutingOn } from "./jev-history";

const settings = (over: Record<string, unknown> = {}) => ({
  jev_squad_routing_mode: "shadow",
  jev_min_confidence: 0.8,
  rule_mode: "off",
  jev_governance_enabled: false,
  ...over,
});

describe("diffSettings", () => {
  it("lists changed settings in a stable order and counts the ones that stayed", () => {
    const diff = diffSettings(
      settings({ jev_squad_routing_mode: "off", jev_min_confidence: 0.7 }),
      settings(),
    );
    expect(diff.changes.map((change) => [change.kind, change.before, change.after])).toEqual([
      ["routing_mode", "off", "shadow"],
      ["min_confidence", 0.7, 0.8],
    ]);
    expect(diff.unchanged).toBe(2);
  });

  it("treats the legacy enabled flag and the mode as one routing setting", () => {
    const diff = diffSettings({ jev_squad_routing_enabled: true }, { jev_squad_routing_mode: "on" });
    expect(diff.changes).toEqual([]);
    expect(diff.unchanged).toBe(1);
  });

  it("reports a first save against no earlier settings, with a missing confidence as null", () => {
    const diff = diffSettings(null, { jev_min_confidence: 0.9, rule_mode: "off" });
    expect(diff.changes.map((change) => [change.kind, change.before, change.after])).toEqual([
      ["min_confidence", null, 0.9],
    ]);
  });

  it("compares limits and unknown keys without printing their values", () => {
    const diff = diffSettings({ limits: { b: 1, a: 2 }, future_flag: 1 }, { limits: { a: 3, b: 1 }, future_flag: 1 });
    expect(diff.changes).toHaveLength(1);
    expect(diff.changes[0]).toMatchObject({ kind: "limits", key: "limits" });
    expect(diffSettings({ limits: { a: 1, b: 2 } }, { limits: { b: 2, a: 1 } }).changes).toEqual([]);
  });
});

describe("restoresRoutingOn", () => {
  it("is true for a version whose settings resolve to on, however they say it", () => {
    const entry = (effective: Record<string, unknown>) => ({
      version: 3,
      actor_id: "u",
      previous: null,
      effective,
      changed_at: "2026-10-15T00:00:00Z",
    });
    expect(restoresRoutingOn(entry({ jev_squad_routing_mode: "on" }))).toBe(true);
    expect(restoresRoutingOn(entry({ jev_squad_routing_enabled: true }))).toBe(true);
    expect(restoresRoutingOn(entry({ jev_squad_routing_mode: "shadow", jev_squad_routing_enabled: true }))).toBe(false);
  });
});

describe("formatConfidence", () => {
  it("prints two decimals and never rounds a stored value", () => {
    expect(formatConfidence(0.8)).toBe("0.80");
    expect(formatConfidence(null)).toBe("0.90");
    expect(formatConfidence(0.855)).toBe("0.855");
  });
});
