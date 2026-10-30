// @vitest-environment node
import { describe, expect, it } from "vitest";
import { parseWithFallback } from "../api/schema";
import {
  EMPTY_ISSUE_WORKFLOW_DRY_RUN,
  IssueWorkflowDryRunResponseSchema,
  IssueWorkflowSchema,
  ListIssueWorkflowsResponseSchema,
  WorkflowHandoffPreviewSchema,
} from "../api/schemas";

const baseWorkflow = {
  id: "wf-1",
  workspace_id: "ws-1",
  name: "Delivery",
  description: "",
  is_default: false,
  steps: [
    { status_key: "todo", handler: { type: "none" }, instructions: "" },
    {
      status_key: "code_review",
      handler: { type: "agent", id: "agent-1" },
      instructions: "Review the PR.",
      next_status_key: "done",
      back_status_key: "todo",
    },
  ],
  project_ids: ["project-1"],
  created_at: "2026-09-24T00:00:00Z",
  updated_at: "2026-09-24T00:00:00Z",
};

describe("workflow schemas (MUL-7420)", () => {
  it("parses a workflow list", () => {
    const parsed = ListIssueWorkflowsResponseSchema.parse({ workflows: [baseWorkflow], total: 1 });
    expect(parsed.workflows[0]?.steps[1]?.handler).toEqual({ type: "agent", id: "agent-1" });
    expect(parsed.workflows[0]?.project_ids).toEqual(["project-1"]);
  });

  it("keeps step edges so a save does not drop them (CHE-2030)", () => {
    const edges = [{ to: "done", guard: "pr.merged", timeout: "72h", on_timeout: "blocked" }];
    const parsed = IssueWorkflowSchema.parse({
      ...baseWorkflow,
      steps: [{ status_key: "code_review", handler: { type: "none" }, instructions: "", edges }],
    });
    expect(parsed.steps[0]?.edges).toEqual(edges);
  });

  it("keeps a sub-workflow step so a save does not drop it (CHE-2031)", () => {
    const sub_workflow = { workflow_id: "wf-2", stage: 1 };
    const parsed = IssueWorkflowSchema.parse({
      ...baseWorkflow,
      steps: [{ status_key: "in_progress", handler: { type: "none" }, instructions: "", sub_workflow }],
    });
    expect(parsed.steps[0]?.sub_workflow).toEqual(sub_workflow);
  });

  it("reads the run rules and defaults them for an older server (CHE-2152)", () => {
    const set = IssueWorkflowSchema.parse({ ...baseWorkflow, max_runs_per_issue: 8, include_child_prs: false });
    expect(set.max_runs_per_issue).toBe(8);
    expect(set.include_child_prs).toBe(false);
    const older = IssueWorkflowSchema.parse(baseWorkflow);
    expect(older.max_runs_per_issue).toBeNull();
    expect(older.include_child_prs).toBe(true);
  });

  it("falls back to an empty workflow list when run rules are malformed (CHE-2152)", () => {
    const parsed = parseWithFallback(
      { workflows: [{ ...baseWorkflow, max_runs_per_issue: "many" }], total: 1 },
      ListIssueWorkflowsResponseSchema,
      { workflows: [], total: 0 },
      { endpoint: "GET /api/workflows" },
    );
    expect(parsed).toEqual({ workflows: [], total: 0 });
  });

  it("degrades an unknown handler type to a manual step", () => {
    const parsed = IssueWorkflowSchema.parse({
      ...baseWorkflow,
      steps: [{ status_key: "todo", handler: { type: "robot", id: "x" } }],
    });
    expect(parsed.steps[0]?.handler.type).toBe("none");
    expect(parsed.steps[0]?.instructions).toBe("");
  });

  it("defaults the optional fields an older server may omit", () => {
    const { description: _d, project_ids: _p, ...minimal } = baseWorkflow;
    const parsed = IssueWorkflowSchema.parse({ ...minimal, steps: [{ status_key: "todo" }] });
    expect(parsed.description).toBe("");
    expect(parsed.project_ids).toEqual([]);
    expect(parsed.steps[0]?.handler.type).toBe("none");
  });

  it("reads a workflow row's status-only steps as manual columns", () => {
    const parsed = IssueWorkflowSchema.parse({ ...baseWorkflow, is_default: true, steps: [{ status_key: "todo" }] });
    expect(parsed.is_default).toBe(true);
    expect(parsed.steps[0]).toEqual({ status_key: "todo", handler: { type: "none" }, instructions: "" });
  });

  it("falls back to an empty plan on a malformed dry run", () => {
    const parsed = parseWithFallback(
      { plan: { required: 5 } },
      IssueWorkflowDryRunResponseSchema,
      EMPTY_ISSUE_WORKFLOW_DRY_RUN,
      { endpoint: "POST /api/projects/{id}/workflow (dry run)" },
    );
    expect(parsed).toEqual(EMPTY_ISSUE_WORKFLOW_DRY_RUN);
  });

  it("parses a handoff preview and degrades an unknown handler type", () => {
    const parsed = WorkflowHandoffPreviewSchema.parse({
      handoff: true,
      workflow_name: "Delivery",
      from_status: "implement",
      to_status: "code_review",
      handler_type: "agent",
      handler_id: "agent-2",
      previous_assignee_type: "agent",
      previous_assignee_id: "agent-1",
      previous_runs: [{ task_id: "t1", agent_id: "agent-1", status: "running", started_at: "2026-09-24T00:00:00Z" }],
      brief: "This project's workflow — Delivery",
    });
    expect(parsed.handler_type).toBe("agent");
    expect(parsed.previous_runs).toHaveLength(1);
    expect(WorkflowHandoffPreviewSchema.parse({ handoff: true, handler_type: "robot" }).handler_type).toBeNull();
  });

  it("does not read a malformed handoff preview as no handoff", () => {
    // A preview the client cannot read must not become "nothing to confirm".
    for (const bad of [{ handoff: "yes", previous_runs: 3 }, {}, null, []]) {
      expect(WorkflowHandoffPreviewSchema.safeParse(bad).success).toBe(false);
    }
  });
});
