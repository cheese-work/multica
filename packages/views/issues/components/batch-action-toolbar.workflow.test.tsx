// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Issue, IssueWorkflow, Project } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { BatchActionToolbar } from "./batch-action-toolbar";

// "Workflow" in the batch toolbar points the selected issues at another
// workflow, or back to their project's (CHE-2822). It follows the server:
// workspace admins and the project's lead only, behind the project-workflows
// flag; the server also refuses an issue with a live task, and that issue is
// named instead of being reported as updated.

const selection = vi.hoisted(() => ({ selectedIds: new Set<string>(), clear: vi.fn() }));
vi.mock("@multica/core/issues/use-issue-deletion", () => ({
  useIssueDeletion: () => ({ canDelete: () => true }),
}));

vi.mock("@multica/core/issues/stores/selection-store", () => ({
  useIssueSelectionStore: (selector: (s: typeof selection) => unknown) => selector(selection),
}));
vi.mock("@multica/core/issues/mutations", () => ({
  useBatchUpdateIssues: () => ({ mutateAsync: vi.fn(), isPending: false }),
  useBatchDeleteIssues: () => ({ mutateAsync: vi.fn(), isPending: false }),
  useUpdateIssue: () => ({ mutateAsync: vi.fn(), isPending: false }),
}));

const api = vi.hoisted(() => ({
  listIssueWorkflows: vi.fn(),
  listProjects: vi.fn(),
  setIssueWorkflow: vi.fn(),
}));
vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api,
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/modals", () => ({
  useModalStore: (selector: (s: { open: () => void }) => unknown) => selector({ open: () => {} }),
}));
const flag = vi.hoisted(() => ({ enabled: true }));
vi.mock("@multica/core/config", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/config")>()),
  useFeatureEnabled: () => flag.enabled,
}));
const permissions = vi.hoisted(() => ({ isAdmin: true, leads: new Set<string>() }));
vi.mock("../../workflows/permissions", () => ({
  useWorkflowPermissions: () => ({
    isAdmin: permissions.isAdmin,
    canCreateWorkflow: true,
    canEditWorkflow: permissions.isAdmin,
    canManageProject: permissions.isAdmin,
    isProjectLead: (project?: { id: string } | null) => !!project && permissions.leads.has(project.id),
  }),
}));
const toast = vi.hoisted(() => ({ success: vi.fn(), warning: vi.fn(), error: vi.fn() }));
vi.mock("sonner", () => ({ toast }));
vi.mock("./pickers", () => ({
  StatusPicker: () => <div />,
  PriorityPicker: () => <div />,
  AssigneePicker: () => <div />,
}));

const workflow = (id: string, name: string): IssueWorkflow =>
  ({
    id,
    workspace_id: "ws-1",
    name,
    description: "",
    is_default: false,
    steps: [{ status_key: "todo", handler: { type: "none" }, instructions: "" }],
    project_ids: [],
    owner_id: null,
    created_at: "",
    updated_at: "",
  });
const PROJECTS = [
  { id: "p-dev", workflow_id: null },
  { id: "p-ops", workflow_id: null },
] as Project[];

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
    assignee_type: null,
    assignee_id: null,
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

const A = makeIssue({ id: "a", identifier: "MUL-1" });
const B = makeIssue({ id: "b", identifier: "MUL-2", workflow_id: "design" });

function mount(issues: Issue[]) {
  selection.selectedIds = new Set(issues.map((i) => i.id));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <BatchActionToolbar issues={issues} />
    </QueryClientProvider>,
  );
}

async function openWorkflowMenu() {
  fireEvent.click(await screen.findByRole("button", { name: "Workflow" }));
}

beforeEach(() => {
  flag.enabled = true;
  permissions.isAdmin = true;
  permissions.leads = new Set();
  api.listIssueWorkflows.mockResolvedValue({ workflows: [workflow("dev", "Dev"), workflow("design", "Design")] });
  api.listProjects.mockResolvedValue({ projects: PROJECTS, total: 2 });
  api.setIssueWorkflow.mockResolvedValue(undefined);
});

afterEach(() => {
  cleanup();
  for (const mock of [...Object.values(api), toast.success, toast.warning, toast.error, selection.clear]) mock.mockReset();
});

describe("BatchActionToolbar Set workflow (CHE-2822)", () => {
  it("points every selected issue at the chosen workflow", async () => {
    mount([A, B]);
    await openWorkflowMenu();
    fireEvent.click(await screen.findByRole("button", { name: "Dev" }));

    await waitFor(() => expect(api.setIssueWorkflow).toHaveBeenCalledTimes(2));
    expect(api.setIssueWorkflow).toHaveBeenCalledWith("a", "dev");
    expect(api.setIssueWorkflow).toHaveBeenCalledWith("b", "dev");
    await waitFor(() => expect(toast.success).toHaveBeenCalled());
  });

  it("returns issues to their project's workflow with null", async () => {
    mount([A, B]);
    await openWorkflowMenu();
    fireEvent.click(await screen.findByRole("button", { name: "Follow the project’s workflow" }));

    await waitFor(() => expect(api.setIssueWorkflow).toHaveBeenCalledTimes(2));
    expect(api.setIssueWorkflow).toHaveBeenCalledWith("a", null);
    expect(api.setIssueWorkflow).toHaveBeenCalledWith("b", null);
  });

  it("offers no project option when a selected issue has no project", async () => {
    mount([A, makeIssue({ id: "c", identifier: "MUL-3", project_id: null })]);
    await openWorkflowMenu();
    await screen.findByRole("button", { name: "Dev" });
    expect(screen.queryByRole("button", { name: "Follow the project’s workflow" })).toBeNull();
  });

  it("names the issues the server refuses and keeps the selection", async () => {
    api.setIssueWorkflow.mockImplementation(async (id: string) => {
      if (id === "b") throw new Error("the issue has a task that can still run");
    });
    mount([A, B]);
    await openWorkflowMenu();
    fireEvent.click(await screen.findByRole("button", { name: "Dev" }));

    await waitFor(() => expect(toast.warning).toHaveBeenCalled());
    expect(String(toast.warning.mock.calls[0]![0])).toContain("MUL-2");
    expect(toast.success).not.toHaveBeenCalled();
    expect(selection.clear).not.toHaveBeenCalled();
  });

  it("reports the server's reason when every issue is refused", async () => {
    api.setIssueWorkflow.mockRejectedValue(new Error("the issue has a task that can still run"));
    mount([A]);
    await openWorkflowMenu();
    fireEvent.click(await screen.findByRole("button", { name: "Dev" }));

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("the issue has a task that can still run"));
  });

  it("is hidden from a member who does not lead every selected issue's project", async () => {
    permissions.isAdmin = false;
    permissions.leads = new Set(["p-dev"]);
    mount([A, makeIssue({ id: "c", identifier: "MUL-3", project_id: "p-ops" })]);
    await waitFor(() => expect(api.listIssueWorkflows).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: "Workflow" })).toBeNull();
  });

  it("is offered to the lead of every selected issue's project", async () => {
    permissions.isAdmin = false;
    permissions.leads = new Set(["p-dev", "p-ops"]);
    mount([A, makeIssue({ id: "c", identifier: "MUL-3", project_id: "p-ops" })]);
    expect(await screen.findByRole("button", { name: "Workflow" })).toBeTruthy();
  });

  it("is hidden while the project-workflows flag is off", async () => {
    flag.enabled = false;
    mount([A]);
    await waitFor(() => expect(api.listIssueWorkflows).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: "Workflow" })).toBeNull();
  });
});
