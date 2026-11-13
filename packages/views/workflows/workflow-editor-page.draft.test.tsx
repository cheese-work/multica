/** @vitest-environment jsdom */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import { setCurrentWorkspace } from "@multica/core/platform";
import { issueStatusKeys } from "@multica/core/issue-statuses/queries";
import { issueWorkflowKeys } from "@multica/core/issue-workflows";
import { projectListOptions } from "@multica/core/projects/queries";
import type { IssueStatusEntry, IssueWorkflow } from "@multica/core/types";
import { renderWithI18n } from "../test/i18n";
import { WorkflowEditorPage } from "./workflow-editor-page";

vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: { updateIssueWorkflow: vi.fn(), createIssueWorkflow: vi.fn(), listIssueWorkflows: vi.fn() },
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
const statuses = KEYS.map(
  (key) =>
    ({
      id: key, workspace_id: "ws-1", key, name: key, description: "",
      category: "started", color: "#888888", is_system: true, archived_at: null,
    }) as IssueStatusEntry,
);
const saved = {
  id: "wf-1",
  workspace_id: "ws-1",
  name: "Delivery",
  description: "Ship it",
  is_default: false,
  steps: KEYS.map((status_key) => ({ status_key, handler: { type: "none" as const }, instructions: "" })),
  project_ids: [],
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
  version: "v1",
} as unknown as IssueWorkflow;

function mount(workflowId: string | null, current: IssueWorkflow = saved) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  qc.setQueryData(issueStatusKeys.list("ws-1"), { statuses });
  qc.setQueryData(projectListOptions("ws-1").queryKey, { projects: [], total: 0 });
  qc.setQueryData(issueWorkflowKeys.list("ws-1"), { workflows: [current], total: 1 });
  return {
    qc,
    ...renderWithI18n(
      <QueryClientProvider client={qc}>
        <WorkflowEditorPage workflowId={workflowId} seed={null} canEdit onBack={() => {}} onOpen={() => {}} />
      </QueryClientProvider>,
    ),
  };
}

beforeEach(async () => {
  localStorage.clear();
  resetAllRegisteredDrafts();
  setCurrentWorkspace("acme", "ws-1");
  await new Promise((r) => queueMicrotask(() => r(null)));
});
afterEach(cleanup);

describe("WorkflowEditorPage unsaved edits", () => {
  it("keeps edits to a saved workflow across a remount", async () => {
    const first = mount("wf-1");
    fireEvent.change(screen.getByDisplayValue("Delivery"), { target: { value: "Delivery v2" } });
    first.unmount();

    mount("wf-1");
    expect(await screen.findByDisplayValue("Delivery v2")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("keeps a new workflow's name across a remount", async () => {
    const first = mount(null);
    fireEvent.change(screen.getByRole("textbox", { name: "Name" }), { target: { value: "Fresh flow" } });
    first.unmount();

    mount(null);
    expect(await screen.findByDisplayValue("Fresh flow")).toBeTruthy();
  });

  it("still flags a conflict after a reload when another admin saved while the edits were open", async () => {
    const first = mount("wf-1");
    fireEvent.change(screen.getByDisplayValue("Delivery"), { target: { value: "Delivery v2" } });
    const theirs = { ...saved, description: "Their change", version: "v2" } as IssueWorkflow;
    act(() => first.qc.setQueryData(issueWorkflowKeys.list("ws-1"), { workflows: [theirs], total: 1 }));
    expect(await screen.findByRole("alert")).toBeTruthy();
    first.unmount();

    mount("wf-1", theirs);
    expect(await screen.findByDisplayValue("Delivery v2")).toBeTruthy();
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Save" }).hasAttribute("disabled")).toBe(true);
  });

  it("keeps the conflict across repeated reloads until the admin chooses", async () => {
    const first = mount("wf-1");
    fireEvent.change(screen.getByDisplayValue("Delivery"), { target: { value: "Delivery v2" } });
    const theirs = { ...saved, description: "Their change", version: "v2" } as IssueWorkflow;
    act(() => first.qc.setQueryData(issueWorkflowKeys.list("ws-1"), { workflows: [theirs], total: 1 }));
    first.unmount();

    const second = mount("wf-1", theirs);
    expect(await screen.findByRole("alert")).toBeTruthy();
    second.unmount();

    mount("wf-1", theirs);
    expect(await screen.findByDisplayValue("Delivery v2")).toBeTruthy();
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Save" }).hasAttribute("disabled")).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Keep my edits" }));
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByRole("button", { name: "Save" }).hasAttribute("disabled")).toBe(false);
  });
});
