// @vitest-environment node
import { describe, expect, it } from "vitest";
import { resolveJevGovernance, routingModeOf } from "./jev-governance";

const at = (settings: Record<string, unknown>, version = 3) =>
  resolveJevGovernance({ version, settings, audit_history: [] });

describe("resolveJevGovernance", () => {
  it("uses an explicit known mode and keeps the version and confidence", () => {
    expect(at({ jev_squad_routing_mode: "shadow", jev_min_confidence: 0.8 })).toEqual({
      version: 3,
      routing_mode: "shadow",
      min_confidence: 0.8,
      api_url: "",
      history: [],
    });
    expect(at({ jev_squad_routing_mode: "on" }).routing_mode).toBe("on");
  });

  it("fails safe to off for an unknown mode, even with the legacy flag", () => {
    expect(at({ jev_squad_routing_mode: "turbo", jev_squad_routing_enabled: true }).routing_mode).toBe("off");
  });

  it("reads the legacy enabled flag as on only when no mode is set", () => {
    expect(at({ jev_squad_routing_enabled: true }).routing_mode).toBe("on");
    expect(at({ jev_squad_routing_mode: "", jev_squad_routing_enabled: true }).routing_mode).toBe("on");
    expect(at({ jev_squad_routing_enabled: false }).routing_mode).toBe("off");
  });

  it("is off with no confidence for a workspace that never saved", () => {
    expect(at({}, 0)).toEqual({ version: 0, routing_mode: "off", min_confidence: null, api_url: "", history: [] });
  });
});

describe("routingModeOf", () => {
  it("resolves a raw settings object like the server, and fails safe", () => {
    expect(routingModeOf({ jev_squad_routing_mode: "shadow" })).toBe("shadow");
    expect(routingModeOf({ jev_squad_routing_enabled: true })).toBe("on");
    expect(routingModeOf({ jev_squad_routing_mode: "turbo", jev_squad_routing_enabled: true })).toBe("off");
    expect(routingModeOf({})).toBe("off");
    expect(routingModeOf(null)).toBe("off");
  });
});
