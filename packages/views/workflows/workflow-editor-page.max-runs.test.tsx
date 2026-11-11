/**
 * @vitest-environment jsdom
 *
 * The editor's "Max runs per issue" field (CHE-5143). The save sends
 * `max_runs_per_issue` only when the field changed: the server accepts a cap
 * change only from an interactive session, so a save that leaves it alone
 * must stay a plain, non-interactive-safe write.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { toast } from "sonner";
import { ApiError } from "@multica/core/api";
import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import { issueStatusKeys } from "@multica/core/issue-statuses/queries";
import { issueWorkflowKeys } from "@multica/core/issue-workflows";
import { projectListOptions } from "@multica/core/projects/queries";
import type { IssueStatusEntry, IssueWorkflow } from "@multica/core/types";
import { renderWithI18n } from "../test/i18n";
import { WorkflowEditorPage } from "./workflow-editor-page";

beforeEach(resetAllRegisteredDrafts);

const apiMock = vi.hoisted(() => ({
  updateIssueWorkflow: vi.fn(),
  listIssueWorkflows: vi.fn(),
  createIssueWorkflow: vi.fn(),
}));
vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: apiMock,
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
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

function workflow(overrides: Partial<IssueWorkflow> = {}): IssueWorkflow {
  return {
    id: "wf-1",
    workspace_id: "ws-1",
    name: "Delivery",
    description: "Ship it",
    is_default: false,
    steps: KEYS.map((status_key) => ({ status_key, handler: { type: "none" as const }, instructions: "" })),
    project_ids: [],
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    version: "v-2026-01-01",
    ...overrides,
  } as IssueWorkflow;
}

function statusEntry(key: string): IssueStatusEntry {
  return {
    id: key, workspace_id: "ws-1", key, name: key, description: "",
    category: "started", color: "#888888", is_system: true, archived_at: null,
  } as IssueStatusEntry;
}

function mount(saved: IssueWorkflow) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  queryClient.setQueryData(issueStatusKeys.list("ws-1"), { statuses: KEYS.map(statusEntry) });
  queryClient.setQueryData(projectListOptions("ws-1").queryKey, { projects: [], total: 0 });
  queryClient.setQueryData(issueWorkflowKeys.list("ws-1"), { workflows: [saved], total: 1 });
  renderWithI18n(
    <QueryClientProvider client={queryClient}>
      <WorkflowEditorPage workflowId="wf-1" seed={null} canEdit onBack={() => {}} onOpen={() => {}} />
    </QueryClientProvider>,
  );
}

const field = () => screen.getByLabelText("Max runs per issue") as HTMLInputElement;
const save = () => screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
const type = (value: string) => fireEvent.change(field(), { target: { value } });
const sentBody = () => apiMock.updateIssueWorkflow.mock.calls[0]![1] as Record<string, unknown>;

describe("max runs per issue field", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    apiMock.updateIssueWorkflow.mockResolvedValue(workflow());
  });
  afterEach(cleanup);

  it("shows the saved cap, and the platform default as the placeholder when none is set", () => {
    mount(workflow({ max_runs_per_issue: 100 }));
    expect(field().value).toBe("100");
    cleanup();
    mount(workflow({ max_runs_per_issue: null }));
    expect(field().value).toBe("");
    expect(field().placeholder).toBe("15");
  });

  it("sends max_runs_per_issue: 25 after the field is set to 25", async () => {
    mount(workflow({ max_runs_per_issue: 100 }));
    type("25");
    fireEvent.click(save());
    await waitFor(() => expect(apiMock.updateIssueWorkflow).toHaveBeenCalledTimes(1));
    expect(sentBody().max_runs_per_issue).toBe(25);
  });

  it("omits max_runs_per_issue when the field is untouched", async () => {
    mount(workflow({ max_runs_per_issue: 100 }));
    fireEvent.change(screen.getByDisplayValue("Ship it"), { target: { value: "Ship it fast" } });
    fireEvent.click(save());
    await waitFor(() => expect(apiMock.updateIssueWorkflow).toHaveBeenCalledTimes(1));
    expect(sentBody()).not.toHaveProperty("max_runs_per_issue");
  });

  it("sends 0 (restore the default) when a set field is emptied", async () => {
    mount(workflow({ max_runs_per_issue: 100 }));
    type("");
    fireEvent.click(save());
    await waitFor(() => expect(apiMock.updateIssueWorkflow).toHaveBeenCalledTimes(1));
    expect(sentBody().max_runs_per_issue).toBe(0);
  });

  it.each(["0", "1001", "-3", "2.5", "abc"])("refuses %s and holds Save", (value) => {
    mount(workflow({ max_runs_per_issue: 100 }));
    type(value);
    expect(screen.getByRole("alert").textContent).toContain("1 to 1000");
    expect(save().disabled).toBe(true);
    fireEvent.click(save());
    expect(apiMock.updateIssueWorkflow).not.toHaveBeenCalled();
  });

  it("accepts the range ends 1 and 1000", () => {
    mount(workflow({ max_runs_per_issue: 100 }));
    type("1");
    expect(save().disabled).toBe(false);
    type("1000");
    expect(save().disabled).toBe(false);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("shows the server text on 403 budget_requires_interactive", async () => {
    const text = "changing a workflow's run budget needs an interactive web or desktop session";
    apiMock.updateIssueWorkflow.mockRejectedValueOnce(
      new ApiError(text, 403, "Forbidden", { code: "budget_requires_interactive" }),
    );
    mount(workflow({ max_runs_per_issue: 100 }));
    type("25");
    fireEvent.click(save());
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith(text));
  });
});
