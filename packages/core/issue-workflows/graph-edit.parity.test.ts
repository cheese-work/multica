// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { IssueWorkflowStep } from "../types";
import { validateWorkflowGraph } from "./graph-edit";
import { workflowNextStatus } from "./queries";

const keys = ["backlog", "todo", "in_progress", "in_review", "blocked", "done", "cancelled"];
const stepsFor = (source: string, patch: Partial<IssueWorkflowStep>): IssueWorkflowStep[] =>
  keys.map((status_key) => ({
    status_key,
    handler: { type: "none" },
    instructions: "",
    ...(status_key === source ? patch : {}),
  }));

describe("server parity", () => {
  it("allows an unchanged legacy CI advance without inventing a mandatory timeout", () => {
    const steps = stepsFor("in_progress", {
      advance_on_ci_green: true,
      next_status_key: "in_review",
    });
    expect(validateWorkflowGraph(steps)).toEqual([]);
  });

  it("allows an agent handoff whose next status is the existing board-order default", () => {
    const steps = stepsFor("in_progress", {
      handler: { type: "agent", id: "0f0e0d0c-0b0a-4909-8807-060504030201" },
    });
    expect(workflowNextStatus({ steps }, "in_progress")).toBe("in_review");
    expect(validateWorkflowGraph(steps)).toEqual([]);
  });

  it("allows a manual project-lead handoff without an automatic exit", () => {
    const steps = stepsFor("in_review", { handler: { type: "project_lead" } });
    expect(validateWorkflowGraph(steps)).toEqual([]);
  });

  it.each(["backlog", "blocked", "done", "cancelled"])(
    "rejects an explicit CI-green edge from %s as the latest A1 server does",
    (source) => {
      const steps = stepsFor(source, {
        edges: [{ to: "in_review", guard: "pr.checks_green", timeout: "72h", on_timeout: "todo" }],
      });
      expect(validateWorkflowGraph(steps).some((issue) => issue.from === source)).toBe(true);
    },
  );
});

describe("PR merge virtual edge", () => {
  it("never reaches the steps a save would send", async () => {
    const { workflowEdges } = await import("./graph");
    const { stepEdges, addStepEdge } = await import("./graph-edit");
    const steps = stepsFor("in_progress", {});
    const before = JSON.stringify(steps);
    expect(workflowEdges({ steps }, "done").some((e) => e.from === "*")).toBe(true);
    // The edge is derived for drawing; no step holds it and none can be read from "*".
    expect(JSON.stringify(steps)).toBe(before);
    expect(stepEdges(steps, "*")).toEqual([]);
    expect(
      JSON.stringify(addStepEdge(steps, "in_progress", "done")).includes("pr_merge_status"),
    ).toBe(false);
    expect(validateWorkflowGraph(steps)).toEqual([]);
  });
});
