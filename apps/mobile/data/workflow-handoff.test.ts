import type { WorkflowHandoffPreview } from "@multica/core/types";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "@/data/api";
import { planStatusChange, previewBinding } from "./workflow-handoff";

// Retrospective tests (added with the implementation, not first): they pin the
// plan the status picker route acts on. The fail-closed mutation they feed is
// covered by the earlier red test in mutations/issue-status-binding.test.ts.

vi.mock("@/data/api", () => ({ api: { previewWorkflowHandoff: vi.fn() } }));

const HANDOFF: WorkflowHandoffPreview = {
  handoff: true,
  workflow_name: "Dev",
  from_status: "todo",
  to_status: "in_review",
  handler_type: "agent",
  handler_id: "sentinel",
  previous_assignee_type: "agent",
  previous_assignee_id: "forge",
  previous_runs: [{ task_id: "t1", agent_id: "forge", status: "running", started_at: null }],
  brief: "",
};
const NO_HANDOFF: WorkflowHandoffPreview = { ...HANDOFF, handoff: false, handler_type: null, handler_id: null, previous_runs: [] };

beforeEach(() => {
  vi.mocked(api.previewWorkflowHandoff).mockReset();
});

describe("previewBinding", () => {
  it("binds who the issue is taken from and who it goes to", () => {
    expect(previewBinding(HANDOFF)).toEqual({
      previous_assignee_type: "agent",
      previous_assignee_id: "forge",
      handler_type: "agent",
      handler_id: "sentinel",
    });
  });

  it("binds a preview that found no handoff to no handler", () => {
    expect(previewBinding({ ...NO_HANDOFF, handler_type: "agent", handler_id: "stale" })).toEqual({
      previous_assignee_type: "agent",
      previous_assignee_id: "forge",
      handler_type: null,
      handler_id: null,
    });
  });
});

describe("planStatusChange", () => {
  it("applies a change the preview found no handoff for, bound to that result", async () => {
    vi.mocked(api.previewWorkflowHandoff).mockResolvedValue(NO_HANDOFF);

    const plan = await planStatusChange("issue-1", "in_review");

    expect(api.previewWorkflowHandoff).toHaveBeenCalledWith("issue-1", "in_review");
    expect(plan).toEqual({
      kind: "apply",
      request: {
        status: "in_review",
        handoff_preview: { previous_assignee_type: "agent", previous_assignee_id: "forge", handler_type: null, handler_id: null },
      },
    });
  });

  it("asks to confirm a handoff and builds the write for each choice", async () => {
    vi.mocked(api.previewWorkflowHandoff).mockResolvedValue(HANDOFF);

    const plan = await planStatusChange("issue-1", "in_review");

    if (plan.kind !== "confirm") throw new Error("expected a confirmation");
    expect(plan.preview).toBe(HANDOFF);
    const handoff_preview = { previous_assignee_type: "agent", previous_assignee_id: "forge", handler_type: "agent", handler_id: "sentinel" };
    expect(plan.request(false)).toEqual({ status: "in_review", handoff_preview });
    expect(plan.request(true)).toEqual({ status: "in_review", handoff_preview, stop_previous_assignee_runs: true });
  });

  it("rejects, writing nothing, when the preview fails", async () => {
    vi.mocked(api.previewWorkflowHandoff).mockRejectedValue(new Error("offline"));

    await expect(planStatusChange("issue-1", "in_review")).rejects.toThrow("offline");
  });
});
