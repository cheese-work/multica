// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { IssueWorkflowLintFinding } from "../types";
import { filterLint, groupLintByStep, lintCounts } from "./lint";

const f = (over: Partial<IssueWorkflowLintFinding>): IssueWorkflowLintFinding => ({
  rule: "WF001",
  level: "error",
  message: "m",
  ...over,
});

const findings = [
  f({ rule: "WF003", level: "error", step: "agent_accepted" }),
  f({ rule: "WF007", level: "warning", step: "in_review" }),
  f({ rule: "WF008", level: "warning", step: "agent_accepted" }),
  f({ rule: "WF013", level: "info" }),
  f({ rule: "WF002", level: "error", step: "done", waived: true, waive_reason: "on purpose" }),
];

describe("workflow lint helpers", () => {
  it("counts by level and skips waived findings", () => {
    expect(lintCounts(findings)).toEqual({ errors: 1, warnings: 2, infos: 1 });
  });

  it("filters by level; all keeps every finding, waived included", () => {
    expect(filterLint(findings, "all")).toHaveLength(5);
    expect(filterLint(findings, "error").map((x) => x.rule)).toEqual(["WF003", "WF002"]);
    expect(filterLint(findings, "info").map((x) => x.rule)).toEqual(["WF013"]);
  });

  it("groups by step in first-seen order, workflow-level findings under an empty step", () => {
    expect(groupLintByStep(findings).map((g) => [g.step, g.findings.map((x) => x.rule)])).toEqual([
      ["agent_accepted", ["WF003", "WF008"]],
      ["in_review", ["WF007"]],
      ["", ["WF013"]],
      ["done", ["WF002"]],
    ]);
  });
});
