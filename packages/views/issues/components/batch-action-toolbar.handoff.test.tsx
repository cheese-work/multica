// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { buildIssueStatusCatalog } from "@multica/core/issue-statuses";
import type { Issue, IssueWorkflow, Project, UpdateIssueRequest, WorkflowHandoffPreview } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { BatchActionToolbar } from "./batch-action-toolbar";

// A batch status change that enters a workflow step with a handler hands every
// such issue to that handler, and can stop the previous assignee's run. Each of
// those needs the same confirmation a single status change gets, and the write
// must carry what was confirmed. Issues that hand nothing off still move in the
// one batch call, only after the confirmation.

const selection = vi.hoisted(() => ({
  selectedIds: new Set<string>(),
  clear: vi.fn(),
}));
vi.mock("@multica/core/issues/stores/selection-store", () => ({
  useIssueSelectionStore: (selector: (s: typeof selection) => unknown) => selector(selection),
}));

const batchUpdate = vi.hoisted(() => vi.fn());
const updateIssue = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/issues/mutations", () => ({
  useBatchUpdateIssues: () => ({ mutateAsync: batchUpdate, isPending: false }),
  useBatchDeleteIssues: () => ({ mutateAsync: vi.fn(), isPending: false }),
  useUpdateIssue: () => ({ mutateAsync: updateIssue, isPending: false }),
}));

const api = vi.hoisted(() => ({
  listIssueWorkflows: vi.fn(),
  listProjects: vi.fn(),
  previewWorkflowHandoff: vi.fn(),
}));
vi.mock("@multica/core/api", () => ({ api }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/modals", () => ({
  useModalStore: (selector: (s: { open: () => void }) => unknown) => selector({ open: () => {} }),
}));
vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () =>
    buildIssueStatusCatalog([
      { id: "cr", workspace_id: "ws-1", key: "code_review", name: "Code review", description: "", category: "started", color: "#a855f7", is_system: false, position: 1, archived_at: null, created_at: "", updated_at: "" },
    ]),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: (_type: string, id: string) => ({ forge: "Forge", sentinel: "Sentinel" })[id] ?? "?" }),
}));
vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: ({ actorId }: { actorId: string }) => <span data-testid={`avatar-${actorId}`} />,
}));
const toast = vi.hoisted(() => ({ success: vi.fn(), warning: vi.fn(), error: vi.fn() }));
vi.mock("sonner", () => ({ toast }));

vi.mock("./pickers", () => ({
  StatusPicker: ({ onUpdate }: { onUpdate: (u: Partial<UpdateIssueRequest>) => void }) => (
    <button data-testid="status-code_review" onClick={() => onUpdate({ status: "code_review" })} />
  ),
  PriorityPicker: () => <div />,
  AssigneePicker: () => <div />,
}));

const WORKFLOW = {
  id: "dev",
  workspace_id: "ws-1",
  name: "Dev",
  description: "",
  is_default: false,
  steps: [
    { status_key: "todo", handler: { type: "none" }, instructions: "" },
    { status_key: "code_review", handler: { type: "agent", id: "sentinel" }, instructions: "" },
    { status_key: "done", handler: { type: "none" }, instructions: "" },
  ],
  project_ids: ["p-dev"],
  created_at: "",
  updated_at: "",
} as IssueWorkflow;
const PROJECT = { id: "p-dev", workflow_id: "dev" } as Project;

const HANDOFF: WorkflowHandoffPreview = {
  handoff: true,
  workflow_name: "Dev",
  from_status: "todo",
  to_status: "code_review",
  handler_type: "agent",
  handler_id: "sentinel",
  previous_assignee_type: "agent",
  previous_assignee_id: "forge",
  previous_runs: [{ task_id: "t1", agent_id: "forge", status: "running", started_at: new Date().toISOString() }],
  brief: "",
};

function makeIssue(overrides: Partial<Issue>): Issue {
  return {
    id: "a",
    workspace_id: "ws-1",
    number: 1,
    identifier: "MUL-1",
    title: "Issue",
    description: null,
    status: "todo",
    priority: "none",
    assignee_type: "agent",
    assignee_id: "forge",
    creator_type: "member",
    creator_id: "user-1",
    parent_issue_id: null,
    project_id: "p-dev",
    position: 1,
    stage: null,
    start_date: null,
    due_date: null,
    metadata: {},
    properties: {},
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

const HANDING_OFF = makeIssue({ id: "a", identifier: "MUL-1" });
const PLAIN = makeIssue({ id: "b", identifier: "MUL-2", project_id: null, assignee_type: null, assignee_id: null });

function mount(issues: Issue[]) {
  selection.selectedIds = new Set(issues.map((i) => i.id));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <BatchActionToolbar issues={issues} />
    </QueryClientProvider>,
  );
}

async function chooseStatus() {
  // The catalogs load first; the picker stub is always on screen.
  await waitFor(() => expect(api.listProjects).toHaveBeenCalled());
  await new Promise((resolve) => setTimeout(resolve, 20));
  fireEvent.click(screen.getByTestId("status-code_review"));
}

beforeEach(() => {
  api.listIssueWorkflows.mockResolvedValue({ workflows: [WORKFLOW] });
  api.listProjects.mockResolvedValue({ projects: [PROJECT], total: 1 });
  api.previewWorkflowHandoff.mockResolvedValue(HANDOFF);
  batchUpdate.mockResolvedValue({ updated: 1 });
  updateIssue.mockResolvedValue({ id: "a" });
});

afterEach(() => {
  cleanup();
  for (const mock of [api.listIssueWorkflows, api.listProjects, api.previewWorkflowHandoff, batchUpdate, updateIssue, toast.success, toast.warning, toast.error]) {
    mock.mockReset();
  }
});

describe("BatchActionToolbar status change that hands issues off", () => {
  it("writes nothing until the handoff is confirmed", async () => {
    mount([HANDING_OFF, PLAIN]);
    await chooseStatus();

    expect(batchUpdate).not.toHaveBeenCalled();
    expect(updateIssue).not.toHaveBeenCalled();
    expect(await screen.findByText(/Move 2 issues to “Code review”\?/)).toBeTruthy();
    expect(batchUpdate).not.toHaveBeenCalled();
    expect(updateIssue).not.toHaveBeenCalled();
    expect(api.previewWorkflowHandoff).toHaveBeenCalledWith("a", "code_review");
    expect(api.previewWorkflowHandoff).not.toHaveBeenCalledWith("b", expect.anything());
  });

  it("writes each handoff with what was confirmed and the rest as one batch", async () => {
    mount([HANDING_OFF, PLAIN]);
    await chooseStatus();
    await screen.findByText(/Move 2 issues to “Code review”\?/);

    fireEvent.click(await screen.findByRole("button", { name: "Move 2 issues" }));

    await waitFor(() => expect(batchUpdate).toHaveBeenCalledTimes(1));
    expect(updateIssue).toHaveBeenCalledTimes(1);
    expect(updateIssue).toHaveBeenCalledWith({
      id: "a",
      status: "code_review",
      stop_previous_assignee_runs: true,
      handoff_preview: {
        previous_assignee_type: "agent",
        previous_assignee_id: "forge",
        handler_type: "agent",
        handler_id: "sentinel",
      },
    });
    expect(batchUpdate).toHaveBeenCalledWith({ ids: ["b"], updates: { status: "code_review" } });
  });

  it("keeps the previous run when the stop box is cleared", async () => {
    mount([HANDING_OFF, PLAIN]);
    await chooseStatus();
    fireEvent.click(await screen.findByRole("checkbox"));
    fireEvent.click(screen.getByRole("button", { name: "Move 2 issues" }));

    await waitFor(() => expect(updateIssue).toHaveBeenCalledTimes(1));
    expect(updateIssue.mock.calls[0]![0]).not.toHaveProperty("stop_previous_assignee_runs");
  });

  it("writes nothing when the confirmation is cancelled", async () => {
    mount([HANDING_OFF, PLAIN]);
    await chooseStatus();
    await screen.findByText(/Move 2 issues to “Code review”\?/);

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    await waitFor(() => expect(screen.queryByText(/Move 2 issues to/)).toBeNull());
    expect(batchUpdate).not.toHaveBeenCalled();
    expect(updateIssue).not.toHaveBeenCalled();
  });

  it("writes nothing while a preview has failed, and offers a retry", async () => {
    api.previewWorkflowHandoff.mockRejectedValueOnce(new Error("preview failed"));
    mount([HANDING_OFF, PLAIN]);
    await chooseStatus();

    expect(await screen.findByRole("alert")).toBeTruthy();
    const confirm = screen.getByRole("button", { name: "Move 2 issues" });
    expect((confirm as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(confirm);
    expect(batchUpdate).not.toHaveBeenCalled();
    expect(updateIssue).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect((screen.getByRole("button", { name: "Move 2 issues" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(screen.getByRole("button", { name: "Move 2 issues" }));
    await waitFor(() => expect(updateIssue).toHaveBeenCalledTimes(1));
  });

  it("binds a preview that found no handoff and moves the issue with it", async () => {
    api.previewWorkflowHandoff.mockResolvedValue({ ...HANDOFF, handoff: false, handler_type: undefined, handler_id: undefined, previous_runs: [] });
    mount([HANDING_OFF, PLAIN]);
    await chooseStatus();

    await waitFor(() => expect(updateIssue).toHaveBeenCalledTimes(1));
    expect(updateIssue).toHaveBeenCalledWith({
      id: "a",
      status: "code_review",
      handoff_preview: {
        previous_assignee_type: "agent",
        previous_assignee_id: "forge",
        handler_type: null,
        handler_id: null,
      },
    });
    expect(batchUpdate).toHaveBeenCalledWith({ ids: ["b"], updates: { status: "code_review" } });
  });

  it("applies a batch with no handoff directly", async () => {
    mount([PLAIN, makeIssue({ id: "c", identifier: "MUL-3", project_id: null })]);
    await chooseStatus();

    await waitFor(() => expect(batchUpdate).toHaveBeenCalledWith({ ids: ["b", "c"], updates: { status: "code_review" } }));
    expect(api.previewWorkflowHandoff).not.toHaveBeenCalled();
    expect(updateIssue).not.toHaveBeenCalled();
  });

  // With a project, "no workflow" and "the workflow list has not loaded" both
  // read as no handoff. The server applies the handoff either way.
  it("writes nothing while the workflows have not loaded", async () => {
    api.listIssueWorkflows.mockReturnValue(new Promise(() => {}));
    mount([HANDING_OFF, PLAIN]);
    await waitFor(() => expect(api.listProjects).toHaveBeenCalled());
    await new Promise((resolve) => setTimeout(resolve, 20));

    fireEvent.click(screen.getByTestId("status-code_review"));
    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(batchUpdate).not.toHaveBeenCalled();
    expect(updateIssue).not.toHaveBeenCalled();
    expect(screen.queryByText(/Move 2 issues to/)).toBeNull();
  });

  it("writes nothing while the projects have not loaded", async () => {
    api.listProjects.mockReturnValue(new Promise(() => {}));
    mount([HANDING_OFF, PLAIN]);
    await waitFor(() => expect(api.listIssueWorkflows).toHaveBeenCalled());
    await new Promise((resolve) => setTimeout(resolve, 20));

    fireEvent.click(screen.getByTestId("status-code_review"));
    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(batchUpdate).not.toHaveBeenCalled();
    expect(updateIssue).not.toHaveBeenCalled();
  });

  it("writes nothing after the projects failed to load", async () => {
    api.listProjects.mockRejectedValue(new Error("projects down"));
    mount([HANDING_OFF, PLAIN]);
    await waitFor(() => expect(api.listProjects).toHaveBeenCalled());
    await new Promise((resolve) => setTimeout(resolve, 50));

    fireEvent.click(screen.getByTestId("status-code_review"));
    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(batchUpdate).not.toHaveBeenCalled();
    expect(updateIssue).not.toHaveBeenCalled();
  });
});
