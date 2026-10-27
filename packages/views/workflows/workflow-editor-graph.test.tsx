/**
 * @vitest-environment jsdom
 *
 * Editing a workflow's exits from the graph panel (CHE-2150 A3b): edits land in
 * the draft, save through the existing update call, and an exit the server
 * would refuse holds Save.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { issueStatusKeys } from "@multica/core/issue-statuses/queries";
import { issueWorkflowKeys } from "@multica/core/issue-workflows";
import { projectListOptions } from "@multica/core/projects/queries";
import type { IssueStatusEntry, IssueWorkflow, IssueWorkflowStep } from "@multica/core/types";
import { renderWithI18n } from "../test/i18n";
import { WorkflowEditorPage } from "./workflow-editor-page";

const apiMock = vi.hoisted(() => ({
  updateIssueWorkflow: vi.fn(),
  listIssueWorkflows: vi.fn(),
  createIssueWorkflow: vi.fn(),
}));
vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: apiMock,
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/paths")>()),
  useCurrentWorkspace: () => ({ id: "ws-1", slug: "acme" }),
}));
vi.mock("@multica/core/workspace/hooks", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/workspace/hooks")>()),
  useActorName: () => ({ getActorName: () => "Someone" }),
}));

const KEYS = ["backlog", "todo", "in_progress", "blocked", "done", "cancelled"];

const workflow = (): IssueWorkflow =>
  ({
    id: "wf-1",
    workspace_id: "ws-1",
    name: "Delivery",
    description: "",
    is_default: false,
    owner_id: null,
    steps: KEYS.map((status_key) => ({ status_key, handler: { type: "none" }, instructions: "" })),
    project_ids: [],
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    version: "v1",
  });

const statusEntry = (key: string) =>
  ({
    id: key, workspace_id: "ws-1", key, name: key, description: "",
    category: "started", color: "#888888", is_system: true, archived_at: null,
  }) as IssueStatusEntry;

function renderPage(canEdit = true) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  queryClient.setQueryData(issueStatusKeys.list("ws-1"), { statuses: KEYS.map(statusEntry) });
  queryClient.setQueryData(projectListOptions("ws-1").queryKey, { projects: [], total: 0 });
  queryClient.setQueryData(issueWorkflowKeys.list("ws-1"), { workflows: [workflow()], total: 1 });
  renderWithI18n(
    <QueryClientProvider client={queryClient}>
      <WorkflowEditorPage workflowId="wf-1" seed={null} canEdit={canEdit} onBack={() => {}} onOpen={() => {}} />
    </QueryClientProvider>,
  );
}

const selectStep = (name: RegExp) =>
  fireEvent.click(screen.getAllByRole("button", { pressed: false, name })[0]!);
const saveButton = () => screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;

async function addExitTo(user: ReturnType<typeof userEvent.setup>, label: string) {
  await user.click(screen.getByRole("combobox", { name: "Add exit" }));
  await user.click(await screen.findByRole("option", { name: label }));
}

beforeEach(() => apiMock.updateIssueWorkflow.mockResolvedValue(workflow()));
afterEach(() => {
  cleanup();
  apiMock.updateIssueWorkflow.mockReset();
});

describe("workflow editor graph panel", () => {
  it("saves an exit added from the panel on its step", async () => {
    const user = userEvent.setup();
    renderPage();
    selectStep(/^In Progress/);
    await addExitTo(user, "Done");
    await waitFor(() => expect(saveButton().disabled).toBe(false));

    await act(async () => {
      fireEvent.click(saveButton());
    });

    const steps = apiMock.updateIssueWorkflow.mock.calls[0]![1].steps as IssueWorkflowStep[];
    expect(steps.find((s) => s.status_key === "in_progress")!.edges).toEqual([
      { to: "done", guard: "agent_done" },
    ]);
  });

  it("holds Save and says why when a PR guard has no timeout", async () => {
    const user = userEvent.setup();
    renderPage();
    selectStep(/^In Progress/);
    await addExitTo(user, "Done");
    await user.click(screen.getByRole("combobox", { name: "Guard" }));
    await user.click(await screen.findByRole("option", { name: "pr.merged" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Timeout" }), { target: { value: "" } });

    expect(screen.getByText(/In Progress needs a timeout|An exit of In Progress needs a timeout/)).toBeTruthy();
    expect(saveButton().disabled).toBe(true);
  });

  it("offers no edits without edit rights", () => {
    renderPage(false);
    selectStep(/^In Progress/);
    expect(screen.queryByRole("combobox", { name: "Add exit" })).toBeNull();
  });
});
