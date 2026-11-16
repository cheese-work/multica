import { describe, expect, it } from "vitest";
import { configToForm, formErrors, formToPatch, parseList } from "./form";
import type { CIWatchConfig } from "./schema";

const saved: CIWatchConfig = {
  repo: "cheese-work/multica", mode: "shadow", branches: ["main", "release/*"], events: ["push", "schedule"],
  workflows_include: ["ci.yml"], workflows_exclude: [], p1_mode: "shadow", p2_mode: "shadow",
  p3_mode: "shadow", p4_mode: "shadow", fix_owner_kind: "squad", fix_owner_id: "s1",
  notify_member_id: "m1", max_reruns_per_original_run: 1, max_new_issues_per_day: 5,
  max_owner_wakes_per_hour: 6, updated_at: null,
};

describe("parseList", () => {
  it("splits on newlines and commas, trims, drops blanks", () => {
    expect(parseList(" main, release/* \n\n  dev ,")).toEqual(["main", "release/*", "dev"]);
  });
});

describe("configToForm", () => {
  it("renders lists as lines and the owner as kind:id", () => {
    expect(configToForm(saved)).toMatchObject({
      mode: "shadow", branches: "main\nrelease/*", events: ["push", "schedule"], workflowsInclude: "ci.yml",
      workflowsExclude: "", fixOwner: "squad:s1", notifyMember: "m1", maxReruns: "1", maxIssues: "5", maxWakes: "6",
    });
  });
  it("uses empty strings for unset owner and member", () => {
    expect(configToForm({ ...saved, fix_owner_kind: null, fix_owner_id: null, notify_member_id: null }))
      .toMatchObject({ fixOwner: "", notifyMember: "" });
  });
});

describe("formToPatch", () => {
  it("is empty when nothing changed", () => {
    expect(formToPatch(saved, configToForm(saved))).toEqual({});
  });
  it("sends only the changed fields", () => {
    const f = { ...configToForm(saved), mode: "act", p2: "act", maxIssues: "9", branches: "main\ndev" };
    expect(formToPatch(saved, f)).toEqual({
      mode: "act", p2_mode: "act", max_new_issues_per_day: 9, branches: ["main", "dev"],
    });
  });
  it("sends fix_owner_kind and fix_owner_id together", () => {
    const f = { ...configToForm(saved), fixOwner: "agent:a9" };
    expect(formToPatch(saved, f)).toEqual({ fix_owner_kind: "agent", fix_owner_id: "a9" });
  });
  it("clears the owner with both null and the member with null", () => {
    const f = { ...configToForm(saved), fixOwner: "", notifyMember: "" };
    expect(formToPatch(saved, f)).toEqual({ fix_owner_kind: null, fix_owner_id: null, notify_member_id: null });
  });
  it("sends the selected events", () => {
    const f = { ...configToForm(saved), events: ["push", "workflow_run"] };
    expect(formToPatch(saved, f)).toEqual({ events: ["push", "workflow_run"] });
  });
});

describe("formErrors", () => {
  const ok = configToForm(saved);
  it("is empty for a valid form", () => {
    expect(formErrors(ok)).toEqual([]);
  });
  it("flags a cap that is not an integer from 0 to 100000", () => {
    expect(formErrors({ ...ok, maxReruns: "-1", maxIssues: "1.5", maxWakes: "100001" }))
      .toEqual(["maxReruns", "maxIssues", "maxWakes"]);
    expect(formErrors({ ...ok, maxReruns: "" })).toEqual(["maxReruns"]);
    expect(formErrors({ ...ok, maxReruns: "0" })).toEqual([]);
  });
  it("flags a list over 50 entries or an entry over 200 characters", () => {
    expect(formErrors({ ...ok, branches: Array.from({ length: 51 }, (_, i) => `b${i}`).join("\n") })).toEqual(["branches"]);
    expect(formErrors({ ...ok, workflowsInclude: "x".repeat(201) })).toEqual(["workflowsInclude"]);
  });
});
