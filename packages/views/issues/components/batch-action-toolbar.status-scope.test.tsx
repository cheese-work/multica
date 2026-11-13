// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { buildIssueStatusCatalog } from "@multica/core/issue-statuses";
import type { Issue, IssueWorkflow, Project } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { BatchActionToolbar } from "./batch-action-toolbar";

// The real StatusPicker under the toolbar: which statuses a selection offers
// when its issues follow different overrides of one project (CHE-2822). The
// picker is not mocked, so the scope the toolbar hands it is what is judged.

const selection = vi.hoisted(() => ({ selectedIds: new Set<string>(), clear: vi.fn() }));
vi.mock("@multica/core/issues/stores/selection-store", () => ({
  useIssueSelectionStore: (selector: (s: typeof selection) => unknown) => selector(selection),
}));
vi.mock("@multica/core/issues/mutations", () => ({
  useBatchUpdateIssues: () => ({ mutateAsync: vi.fn(), isPending: false }),
  useBatchDeleteIssues: () => ({ mutateAsync: vi.fn(), isPending: false }),
  useUpdateIssue: () => ({ mutateAsync: vi.fn(), isPending: false }),
}));
// Only the status picker is real; the others need unrelated stores.
vi.mock("./pickers", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./pickers")>()),
  PriorityPicker: () => <div />,
  AssigneePicker: () => <div />,
}));
const api = vi.hoisted(() => ({ listIssueWorkflows: vi.fn(), listProjects: vi.fn() }));
vi.mock("@multica/core/api", () => ({ api }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/modals", () => ({
  useModalStore: (selector: (s: { open: () => void }) => unknown) => selector({ open: () => {} }),
}));
vi.mock("@multica/core/config", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/config")>()),
  useFeatureEnabled: () => false,
}));
vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () =>
    buildIssueStatusCatalog([
      { id: "review", workspace_id: "ws-1", key: "review", name: "Peer review", description: "", category: "started", color: "#a855f7", is_system: false, position: 1, archived_at: null, created_at: "", updated_at: "" },
      { id: "qa", workspace_id: "ws-1", key: "qa", name: "Quality check", description: "", category: "started", color: "#ec7a2d", is_system: false, position: 2, archived_at: null, created_at: "", updated_at: "" },
    ]),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: () => "?" }),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), warning: vi.fn(), error: vi.fn() } }));

const workflow = (id: string, keys: string[]): IssueWorkflow => ({
  id,
  workspace_id: "ws-1",
  name: id,
  description: "",
  is_default: false,
  steps: keys.map((status_key) => ({ status_key, handler: { type: "none" }, instructions: "" })),
  project_ids: [],
  owner_id: null,
  created_at: "",
  updated_at: "",
});

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
    project_id: "p",
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

function rows(): string[] {
  return Array.from(document.querySelectorAll<HTMLElement>("button[data-picker-item]")).map((el) => el.textContent?.trim() ?? "");
}

async function openStatusPicker(issues: Issue[]) {
  selection.selectedIds = new Set(issues.map((i) => i.id));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithI18n(
    <QueryClientProvider client={client}>
      <BatchActionToolbar issues={issues} />
    </QueryClientProvider>,
  );
  await waitFor(() => expect(api.listIssueWorkflows).toHaveBeenCalled());
  await new Promise((resolve) => setTimeout(resolve, 50));
  fireEvent.click(screen.getByRole("button", { name: "Status" }));
  await waitFor(() => expect(rows().length).toBeGreaterThan(0));
}

beforeEach(() => {
  api.listIssueWorkflows.mockResolvedValue({
    workflows: [
      workflow("project-flow", ["todo", "done"]),
      workflow("flow-a", ["todo", "review", "qa"]),
      workflow("flow-b", ["todo", "review"]),
    ],
  });
  api.listProjects.mockResolvedValue({ projects: [{ id: "p", workflow_id: "project-flow" } as Project], total: 1 });
});

afterEach(() => {
  cleanup();
  api.listIssueWorkflows.mockReset();
  api.listProjects.mockReset();
});

describe("BatchActionToolbar status list for mixed overrides (CHE-2822)", () => {
  it("offers the statuses every override shares, even those the project's workflow lacks", async () => {
    await openStatusPicker([
      makeIssue({ id: "a", workflow_id: "flow-a" }),
      makeIssue({ id: "b", workflow_id: "flow-b" }),
    ]);

    expect(rows()).toEqual(expect.arrayContaining(["Todo", "Peer review"]));
    expect(rows()).not.toContain("Done");
    expect(rows()).not.toContain("Quality check");
  });

  it("lists a shared override's own workflow", async () => {
    await openStatusPicker([
      makeIssue({ id: "a", workflow_id: "flow-a" }),
      makeIssue({ id: "b", workflow_id: "flow-a" }),
    ]);

    expect(rows()).toEqual(expect.arrayContaining(["Todo", "Peer review", "Quality check"]));
    expect(rows()).not.toContain("Done");
  });
});
