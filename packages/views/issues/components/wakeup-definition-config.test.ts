import { describe, expect, it } from "vitest";
import type { IssueWakeupInput } from "@multica/core/types";
import { definitionConfigFromInput, draftFromDefinitionConfig } from "./wakeup-definition-config";

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

describe("draftFromDefinitionConfig", () => {
  const roundTrip = (input: IssueWakeupInput) => {
    const built = definitionConfigFromInput(input, "R");
    if ("error" in built) throw new Error(built.error);
    return draftFromDefinitionConfig(built.config);
  };

  it("reads an event rule back into the fields that wrote it", () => {
    expect(roundTrip(base)).toEqual({
      name: "R",
      draft: { condition: "custom", events: ["issue.status_changed"], agentId: "agent-1", instruction: "Check it", mode: "once", waitDays: 7 },
    });
  });

  it("reads each platform condition the editor offers", () => {
    const cond = (condition: IssueWakeupInput["condition"]) =>
      roundTrip({ ...base, event_types: undefined, condition })?.draft;
    expect(cond({ type: "issue_field", field: "status", value: "done" })).toMatchObject({ condition: "field", field: "status", fieldTarget: "done" });
    expect(cond({ type: "issue_field", field: "assignee", assignee_type: "agent", assignee_id: "a" })).toMatchObject({ field: "assignee", assignee: { type: "agent", id: "a" } });
    expect(cond({ type: "children_done", stage: 2 })).toMatchObject({ condition: "children", stage: 2 });
    expect(cond({ type: "children_done" })).toMatchObject({ condition: "children", stage: null });
    expect(cond({ type: "pull_request", event: "merged" })).toMatchObject({ condition: "pull_request", prEvent: "merged" });
  });

  it("keeps a continuous rule's fire limit", () => {
    expect(roundTrip({ ...base, mode: "continuous", max_fires: 10 })?.draft).toMatchObject({ mode: "continuous", maxFires: 10 });
  });

  it("refuses a rule the editor would turn into a different one", () => {
    expect(roundTrip({ ...base, event_types: undefined, condition: { type: "other_issue", issue_id: "i", state: "done" } })).toBeNull();
    expect(roundTrip({ ...base, expires_in_seconds: 2 * 86400 })).toBeNull();
    expect(roundTrip({ ...base, expires_in_seconds: undefined })).toBeNull();
    expect(roundTrip({ ...base, mode: "continuous", max_fires: 7 })).toBeNull();
    expect(roundTrip({ ...base, mode: "continuous" })).toBeNull();
    expect(draftFromDefinitionConfig({ v: 1, name: "R", instruction: "x", mode: "once", trigger: { kind: "event" }, target: { type: "assignee" }, expiry: { after_seconds: 86400 } })).toBeNull();
    expect(draftFromDefinitionConfig({ v: 1, name: "R", instruction: "x", mode: "once", trigger: { kind: "event", events: ["issue.updated"] }, target: { type: "agent", id: "a" }, expiry: { at: "2030-01-01T00:00:00Z" } })).toBeNull();
  });
});
