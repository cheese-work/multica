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
