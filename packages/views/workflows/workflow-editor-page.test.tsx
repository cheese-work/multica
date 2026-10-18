/**
 * @vitest-environment jsdom
 *
 * The editor keeps unsaved edits when the saved workflow refreshes under it.
 * A refresh that changes only bookkeeping (updated_at, the status count) leaves
 * the edits alone. A refresh that changes the workflow's content, because
 * another admin saved it, keeps the edits too, says so, and holds Save until
 * the editor chooses between the two versions.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { issueStatusKeys } from "@multica/core/issue-statuses/queries";
import { issueWorkflowKeys } from "@multica/core/issue-workflows";
import { projectListOptions } from "@multica/core/projects/queries";
import type { IssueStatusEntry, IssueWorkflow } from "@multica/core/types";
import { renderWithI18n } from "../test/i18n";
import { WorkflowEditorPage } from "./workflow-editor-page";

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/paths")>()),
  useCurrentWorkspace: () => ({ id: "ws-1", slug: "acme" }),
}));
vi.mock("@multica/core/workspace/hooks", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/workspace/hooks")>()),
  useActorName: () => ({ getActorName: () => "Someone" }),
}));

function workflow(overrides: Partial<IssueWorkflow> = {}): IssueWorkflow {
  const step = (status_key: string) => ({
    status_key,
    handler: { type: "none" as const },
    instructions: "",
  });
  return {
    id: "wf-1",
    workspace_id: "ws-1",
    name: "Delivery",
    description: "Ship it",
    is_default: false,
    steps: ["backlog", "todo", "in_progress", "blocked", "done", "cancelled"].map(step),
    project_ids: [],
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  } as IssueWorkflow;
}

function statusEntry(key: string): IssueStatusEntry {
  return {
    id: key, workspace_id: "ws-1", key, name: key, description: "",
    category: "started", color: "#888888", is_system: true, archived_at: null,
  } as IssueStatusEntry;
}

describe("workflow editor baseline refresh", () => {
  let queryClient: QueryClient;
  const statuses = ["backlog", "todo", "in_progress", "blocked", "done", "cancelled"].map(statusEntry);

  // Query observers are notified on a later tick, so flush it inside act.
  const settle = () => new Promise((resolve) => setTimeout(resolve, 0));
  const publish = (next: IssueWorkflow) =>
    act(async () => {
      queryClient.setQueryData(issueWorkflowKeys.list("ws-1"), { workflows: [next], total: 1 });
      await settle();
    });

  beforeEach(() => {
    queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    queryClient.setQueryData(issueStatusKeys.list("ws-1"), { statuses });
    queryClient.setQueryData(projectListOptions("ws-1").queryKey, { projects: [], total: 0 });
    queryClient.setQueryData(issueWorkflowKeys.list("ws-1"), { workflows: [workflow()], total: 1 });
    renderWithI18n(
      <QueryClientProvider client={queryClient}>
        <WorkflowEditorPage workflowId="wf-1" seed={null} canEdit onBack={() => {}} onOpen={() => {}} />
      </QueryClientProvider>,
    );
  });

  afterEach(cleanup);

  const nameField = () => screen.getByDisplayValue(/Delivery/) as HTMLInputElement;
  const saveButton = () => screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;

  it("keeps dirty edits when only updated_at changes", async () => {
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });
    expect(saveButton().disabled).toBe(false);

    await publish(workflow({ updated_at: "2026-01-02T00:00:00Z" }));

    expect(screen.getByDisplayValue("Delivery v2")).toBeTruthy();
    expect(saveButton().disabled).toBe(false);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("keeps dirty edits when a status is added to the catalog", async () => {
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });

    await act(async () => {
      queryClient.setQueryData(issueStatusKeys.list("ws-1"), { statuses: [...statuses, statusEntry("in_review")] });
      await settle();
    });

    expect(screen.getByDisplayValue("Delivery v2")).toBeTruthy();
    expect(saveButton().disabled).toBe(false);
  });

  it("keeps dirty edits and holds Save when another admin changes the workflow", async () => {
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });

    await publish(workflow({ description: "Changed elsewhere", updated_at: "2026-01-02T00:00:00Z" }));

    expect(screen.getByDisplayValue("Delivery v2")).toBeTruthy();
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(saveButton().disabled).toBe(true);
  });

  it("loads the latest version on request and drops the edits", async () => {
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });
    await publish(workflow({ name: "Delivery by admin", updated_at: "2026-01-02T00:00:00Z" }));

    fireEvent.click(screen.getByRole("button", { name: "Load latest" }));

    expect(screen.getByDisplayValue("Delivery by admin")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("keeps the edits on request and re-enables Save", async () => {
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });
    await publish(workflow({ description: "Changed elsewhere", updated_at: "2026-01-02T00:00:00Z" }));

    fireEvent.click(screen.getByRole("button", { name: "Keep my edits" }));

    expect(screen.getByDisplayValue("Delivery v2")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(saveButton().disabled).toBe(false);
  });

  it("refreshes a clean editor to the new version", async () => {
    await publish(workflow({ name: "Delivery by admin", updated_at: "2026-01-02T00:00:00Z" }));

    expect(screen.getByDisplayValue("Delivery by admin")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
