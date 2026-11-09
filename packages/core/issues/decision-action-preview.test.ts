import { describe, expect, it } from "vitest";
import { approvableConfigChange, CONFIG_CHANGE_FIELDS } from "./decision-action-preview";
import type { DecisionConfigChange, DecisionRequestAction } from "../types/decision";

const digest = "a".repeat(64);
const change: DecisionConfigChange = {
  field: "workflow.instructions",
  target_id: "wf-1",
  keys: [],
  diff_kind: "text",
  diff: [{ old_start: 1, new_start: 1, lines: ["-old", "+new"] }],
  after: "new",
  before_sha256: "b".repeat(64),
  after_sha256: "c".repeat(64),
  sha256: digest,
};
const action = (patch: Partial<DecisionRequestAction> = {}): DecisionRequestAction => ({
  id: "a1",
  action_type: "workflow.step.instructions.set",
  payload_sha256: "p",
  config_change: change,
  ...patch,
});

describe("approvableConfigChange", () => {
  it("returns a well-formed Scope A change", () => {
    expect(approvableConfigChange(action())).toEqual(change);
  });

  it.each(CONFIG_CHANGE_FIELDS)("accepts the field %s", (field) => {
    const keys = field === "workspace.settings" ? ["compact_prompt"] : [];
    expect(approvableConfigChange(action({ config_change: { ...change, field, keys } }))).toBeDefined();
  });

  it.each([
    "project.description", "members.roles", "passkeys", "workspace.delete", "workspace.delegated_writes",
    "workspace.routing_agent", "agent.skills", "", "future.field",
  ])("fails closed for the field %s", (field) => {
    expect(approvableConfigChange(action({ config_change: { ...change, field } }))).toBeUndefined();
  });

  it("fails closed without a change or with a bad digest", () => {
    expect(approvableConfigChange(action({ config_change: undefined }))).toBeUndefined();
    expect(approvableConfigChange(action({ config_change: { ...change, sha256: "abc" } }))).toBeUndefined();
    expect(approvableConfigChange(action({ config_change: { ...change, sha256: "A".repeat(64) } }))).toBeUndefined();
  });

  it("needs keys for a settings change and none for another field", () => {
    expect(approvableConfigChange(action({ config_change: { ...change, field: "workspace.settings", keys: [] } }))).toBeUndefined();
    expect(approvableConfigChange(action({ config_change: { ...change, keys: ["compact_prompt"] } }))).toBeUndefined();
  });

  it("fails closed for an action type outside the config-change types", () => {
    expect(approvableConfigChange(action({ action_type: "agent.env.patch" }))).toBeUndefined();
    expect(approvableConfigChange(action({ action_type: "future.thing" }))).toBeUndefined();
  });

  it("fails closed for a malformed diff", () => {
    expect(approvableConfigChange(action({ config_change: { ...change, diff: [{ old_start: 1, new_start: 1, lines: ["x"] }] } }))).toBeUndefined();
    expect(approvableConfigChange(action({ config_change: { ...change, diff_kind: "other" as never } }))).toBeUndefined();
  });
});
