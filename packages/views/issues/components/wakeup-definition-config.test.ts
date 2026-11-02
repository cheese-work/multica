import { describe, expect, it } from "vitest";
import type { IssueWakeupInput } from "@multica/core/types";
import { definitionConfigFromInput } from "./wakeup-definition-config";

const base: IssueWakeupInput = {
  agent_id: "agent-1",
  instruction: "Check it",
  kind: "event",
  mode: "once",
  expires_in_seconds: 7 * 86400,
  on_timeout: "wake",
  event_types: ["issue.status_changed"],
};

describe("definitionConfigFromInput", () => {
  it("turns an event rule into a sparse workspace definition", () => {
    expect(definitionConfigFromInput(base, " Triage ")).toEqual({
      config: {
        v: 1,
        name: "Triage",
        trigger: { kind: "event", events: ["issue.status_changed"] },
        target: { type: "agent", id: "agent-1" },
        instruction: "Check it",
        mode: "once",
        expiry: { after_seconds: 604800 },
      },
    });
  });

  it("carries the platform condition and the fire cap of a continuous rule", () => {
    const result = definitionConfigFromInput(
      { ...base, event_types: undefined, condition: { type: "pull_request", event: "merged" }, mode: "continuous", max_fires: 5 },
      "PR",
    );
    expect(result).toMatchObject({
      config: { trigger: { kind: "condition", condition: { type: "pull_request", event: "merged" } }, mode: "continuous", max_fires: 5 },
    });
  });

  it("refuses a name that is empty or too long", () => {
    expect(definitionConfigFromInput(base, "  ")).toEqual({ error: "missing_name" });
    expect(definitionConfigFromInput(base, "x".repeat(121))).toEqual({ error: "missing_name" });
  });

  it("refuses author and agent filters a definition cannot store", () => {
    expect(definitionConfigFromInput({ ...base, filter_actor_type: "member", filter_actor_id: "u" }, "n")).toEqual({ error: "unsupported_filter" });
    expect(definitionConfigFromInput({ ...base, filter_agent_id: "a" }, "n")).toEqual({ error: "unsupported_filter" });
  });

  it("refuses a schedule, which only an issue rule has", () => {
    expect(definitionConfigFromInput({ ...base, kind: "at", at: "2030-01-01T00:00:00Z" }, "n")).toEqual({ error: "unsupported_schedule" });
  });
});
