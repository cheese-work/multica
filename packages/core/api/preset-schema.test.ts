// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  AgentPresetListSchema,
  AgentTaskSchema,
  ChatSessionSchema,
  PresetCompletionsSchema,
} from "./schemas";

const uuid = "11111111-1111-4111-8111-111111111111";

describe("agent preset responses", () => {
  it("defaults the optional fields of a preset row", () => {
    const [p] = AgentPresetListSchema.parse([{ name: "fast" }]);
    expect(p).toMatchObject({
      name: "fast",
      model: null,
      thinking_level: null,
      service_tier: null,
      context_args: null,
      is_default: false,
      stale: "",
    });
  });

  it("keeps an unknown stale reason instead of dropping the row", () => {
    const [p] = AgentPresetListSchema.parse([{ name: "x", stale: "future_reason" }]);
    expect(p?.stale).toBe("future_reason");
  });

  it("reads the completion names and tolerates a malformed body", () => {
    expect(PresetCompletionsSchema.parse({ names: ["base", "reset", "fast"] }).names).toEqual([
      "base",
      "reset",
      "fast",
    ]);
    expect(PresetCompletionsSchema.parse({ names: 7 }).names).toEqual([]);
  });
});

describe("chat session preset_name", () => {
  const session = { id: uuid };
  it("is null for inherit, a string for an explicit choice, absent from old servers", () => {
    expect(ChatSessionSchema.parse({ ...session, preset_name: null }).preset_name).toBeNull();
    expect(ChatSessionSchema.parse({ ...session, preset_name: "base" }).preset_name).toBe("base");
    expect(ChatSessionSchema.parse(session).preset_name).toBeUndefined();
    expect(ChatSessionSchema.parse({ ...session, preset_name: 3 }).preset_name).toBeUndefined();
  });
});

describe("task preset_run", () => {
  const run = {
    name: "fast",
    source: "dm",
    skips: [{ level: "issue_label", name: "old", reason: "lab_lock" }],
    claimed: { model: "m", thinking_level: "high", service_tier: null, context_args: ["--a"] },
    applied: null,
  };
  it("parses claimed with applied still null", () => {
    const t = AgentTaskSchema.parse({ id: "t", preset_run: run });
    expect(t.preset_run?.claimed.model).toBe("m");
    expect(t.preset_run?.applied).toBeNull();
    expect(t.preset_run?.skips[0]?.reason).toBe("lab_lock");
  });
  it("keeps applied apart from claimed", () => {
    const t = AgentTaskSchema.parse({
      id: "t",
      preset_run: { ...run, applied: { model: "other", thinking_level: null, service_tier: null, context_args: [], reported_at: "2026-11-01T00:00:00Z" } },
    });
    expect(t.preset_run?.claimed.model).toBe("m");
    expect(t.preset_run?.applied?.model).toBe("other");
  });
  it("absent or malformed preset_run costs only the preset block", () => {
    expect(AgentTaskSchema.parse({ id: "t" }).preset_run).toBeUndefined();
    const t = AgentTaskSchema.parse({ id: "t", preset_run: "nope" });
    expect(t.id).toBe("t");
    expect(t.preset_run).toBeUndefined();
  });
});
