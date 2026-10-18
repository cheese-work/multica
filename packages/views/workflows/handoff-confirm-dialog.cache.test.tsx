// @vitest-environment jsdom

import { act, cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import { buildIssueStatusCatalog } from "@multica/core/issue-statuses";
import { workflowHandoffPreviewOptions } from "@multica/core/issue-workflows";
import type { WorkflowHandoffPreview } from "@multica/core/types";
import { renderWithI18n } from "../test/i18n";
import { WorkflowHandoffConfirmDialog } from "./handoff-confirm-dialog";

const previewWorkflowHandoff = vi.fn<(issueId: string, status: string) => Promise<WorkflowHandoffPreview>>();

vi.mock("@multica/core/api", () => ({
  api: { previewWorkflowHandoff: (issueId: string, status: string) => previewWorkflowHandoff(issueId, status) },
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () =>
    buildIssueStatusCatalog([
      { id: "b", workspace_id: "ws-1", key: "code_review", name: "Code review", description: "", category: "started", color: "#a855f7", is_system: false, position: 2, archived_at: null, created_at: "", updated_at: "" },
    ]),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: (_type: string, id: string) => id }),
}));
vi.mock("../common/actor-avatar", () => ({ ActorAvatar: () => <span /> }));

const noHandoff: WorkflowHandoffPreview = {
  handoff: false, workflow_name: "", from_status: "", to_status: "", handler_type: null, handler_id: null,
  previous_assignee_type: null, previous_assignee_id: null, previous_runs: [], brief: "",
};
const handoff: WorkflowHandoffPreview = {
  ...noHandoff, handoff: true, workflow_name: "Delivery", from_status: "implement", to_status: "code_review",
  handler_type: "agent", handler_id: "sentinel", brief: "Review the PR.",
};

// The dialog is opened for an issue and destination whose last preview is still
// cached (the cache keeps results for 30 seconds). That result describes the
// workflow as it was then. It must not apply the move before the preview for
// this opening has been fetched.
function renderWithCachedNoHandoff() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(workflowHandoffPreviewOptions("ws-1", "issue-1", "code_review").queryKey, noHandoff);
  const onConfirm = vi.fn();
  renderWithI18n(
    <QueryClientProvider client={qc}>
      <WorkflowHandoffConfirmDialog
        issue={{ id: "issue-1", identifier: "MUL-8121" }}
        toStatus="code_review"
        onCancel={vi.fn()}
        onConfirm={onConfirm}
      />
    </QueryClientProvider>,
  );
  return { onConfirm };
}

const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

afterEach(() => {
  cleanup();
  previewWorkflowHandoff.mockReset();
});

describe("WorkflowHandoffConfirmDialog with a cached non-handoff preview", () => {
  it("does not apply the move while the refresh for this opening is pending", async () => {
    previewWorkflowHandoff.mockReturnValue(new Promise(() => {}));
    const { onConfirm } = renderWithCachedNoHandoff();
    await act(async () => settle());

    expect(previewWorkflowHandoff).toHaveBeenCalledTimes(1);
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it("does not apply the move when the refresh fails, and offers a retry", async () => {
    previewWorkflowHandoff.mockRejectedValue(new Error("network down"));
    const { onConfirm } = renderWithCachedNoHandoff();

    expect(await screen.findByRole("button", { name: "Retry" })).toBeTruthy();
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it("asks for confirmation when the refresh finds a handoff that was not there before", async () => {
    previewWorkflowHandoff.mockResolvedValue(handoff);
    const { onConfirm } = renderWithCachedNoHandoff();

    expect(await screen.findByRole("button", { name: "Move to Code review" })).toBeTruthy();
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it("applies the move once the refresh confirms there is no handoff", async () => {
    previewWorkflowHandoff.mockResolvedValue(noHandoff);
    const { onConfirm } = renderWithCachedNoHandoff();

    await waitFor(() => expect(onConfirm).toHaveBeenCalledWith({ stopPreviousRuns: false }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it("applies the move after a failed refresh is retried and finds no handoff", async () => {
    previewWorkflowHandoff.mockRejectedValueOnce(new Error("network down")).mockResolvedValue(noHandoff);
    const { onConfirm } = renderWithCachedNoHandoff();
    fireEvent.click(await screen.findByRole("button", { name: "Retry" }));

    await waitFor(() => expect(onConfirm).toHaveBeenCalledWith({ stopPreviousRuns: false }));
  });
});
