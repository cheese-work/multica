// @vitest-environment jsdom

import { cleanup, fireEvent, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import { projectListOptions } from "@multica/core/projects/queries";
import type { IssueWorkflow, Project } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { IssueProjectPicker } from "./issue-project-picker";

const PREVIEW = { previous_assignee_type: "agent", previous_assignee_id: "forge", handler_type: "agent", handler_id: "sentinel" };

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("./project-picker", () => ({
  ProjectPicker: ({ onUpdate }: { onUpdate: (u: { project_id: string }) => void }) => (
    <button onClick={() => onUpdate({ project_id: "project-2" })}>pick project</button>
  ),
}));
vi.mock("../../workflows/handoff-confirm-dialog", () => ({
  WorkflowHandoffConfirmDialog: ({ issue, onConfirm }: { issue: unknown; onConfirm: (o: unknown) => void }) =>
    issue ? <button onClick={() => onConfirm({ stopPreviousRuns: true, preview: PREVIEW })}>confirm handoff</button> : null,
}));

const workflow = {
  id: "wf-1", workspace_id: "ws-1", name: "Delivery", description: "", is_default: false, project_ids: ["project-2"],
  created_at: "", updated_at: "",
  steps: [
    { status_key: "todo", handler: { type: "agent", id: "sentinel" }, instructions: "" },
    { status_key: "done", handler: { type: "none" }, instructions: "" },
  ],
} as IssueWorkflow;
vi.mock("@multica/core/issue-workflows", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/issue-workflows")>()),
  useIssueWorkflows: () => ({ workflows: [workflow], isLoaded: true, isError: false, retry: () => {} }),
}));

afterEach(cleanup);

describe("IssueProjectPicker confirmed handoff", () => {
  it("sends the previewed binding with the project move", () => {
    const qc = new QueryClient();
    qc.setQueryData(projectListOptions("ws-1").queryKey, {
      projects: [{ id: "project-2", workspace_id: "ws-1", title: "Delivery project", workflow_id: "wf-1" } as Project],
      total: 1,
    });
    const onUpdate = vi.fn();
    renderWithI18n(
      <QueryClientProvider client={qc}>
        <IssueProjectPicker
          issue={{ id: "issue-1", identifier: "TES-1", status: "todo", project_id: "project-1" }}
          projectId="project-1"
          onUpdate={onUpdate}
        />
      </QueryClientProvider>,
    );

    fireEvent.click(screen.getByText("pick project"));
    fireEvent.click(screen.getByText("confirm handoff"));

    expect(onUpdate).toHaveBeenCalledWith({
      project_id: "project-2",
      enter_workflow_step: true,
      stop_previous_assignee_runs: true,
      handoff_preview: PREVIEW,
    });
  });
});
