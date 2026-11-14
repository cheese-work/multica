import { describe, expect, it } from "vitest";
import type { IssueWorkflowStep } from "../types";
import { workflowEdges } from "./graph";

const step = (
  status_key: string,
  rest: Partial<IssueWorkflowStep> = {},
): IssueWorkflowStep => ({
  status_key,
  handler: { type: "none" },
  instructions: "",
  ...rest,
});

describe("workflowEdges", () => {
  it("returns a step's own edges when it has any", () => {
    const steps = [
      step("todo"),
      step("in_review", {
        next_status_key: "cancelled",
        edges: [
          {
            to: "done",
            guard: "pr.merged",
            timeout: "72h",
            on_timeout: "blocked",
          },
        ],
      }),
      step("done"),
    ];
    expect(
      workflowEdges({ steps }).filter((e) => e.from === "in_review"),
    ).toEqual([
      {
        id: "in_review#0",
        index: 0,
        from: "in_review",
        to: "done",
        guard: "pr.merged",
        timeout: "72h",
        on_timeout: "blocked",
      },
    ]);
  });

  it("derives edges from next, back and CI-green like the server", () => {
    const steps = [
      step("todo"),
      step("in_progress", { next_status_key: "in_review" }),
      step("in_review", {
        back_status_key: "in_progress",
        advance_on_ci_green: true,
      }),
      step("done"),
    ];
    const edges = workflowEdges({ steps }).map(
      ({ from, to, guard }) => `${from}>${to}:${guard}`,
    );
    expect(edges).toEqual([
      "in_progress>in_review:agent_done",
      "in_review>in_progress:agent_verdict(CHANGES)",
      "in_review>done:pr.checks_green",
    ]);
  });

  it("does not derive an agent_done exit for a step with no own next", () => {
    const steps = [step("todo"), step("in_progress"), step("done")];
    expect(workflowEdges({ steps })).toEqual([]);
  });

  it("drops edges to a status the workflow does not list", () => {
    const steps = [
      step("todo", { edges: [{ to: "gone", guard: "agent_done" }] }),
      step("done"),
    ];
    expect(workflowEdges({ steps })).toEqual([]);
  });
});

describe("workflowEdges: PR merge rule", () => {
  const listed = [step("todo"), step("in_review"), step("done")];
  const virtual = (edges: ReturnType<typeof workflowEdges>) =>
    edges.filter((e) => e.from === "*");

  it("adds one read-only edge from any step to the status the workflow lists", () => {
    expect(virtual(workflowEdges({ steps: listed }, "done"))).toEqual([
      expect.objectContaining({
        from: "*",
        to: "done",
        guard: "pr_merge_status",
        readOnly: true,
      }),
    ]);
  });

  it("adds none for `none`, an unlisted status, or no setting", () => {
    expect(virtual(workflowEdges({ steps: listed }, "none"))).toEqual([]);
    expect(virtual(workflowEdges({ steps: listed }, "merged"))).toEqual([]);
    expect(virtual(workflowEdges({ steps: listed }))).toEqual([]);
  });

  it("leaves the step edges unchanged", () => {
    expect(
      workflowEdges({ steps: listed }, "done").filter((e) => e.from !== "*"),
    ).toEqual(workflowEdges({ steps: listed }));
  });
});
