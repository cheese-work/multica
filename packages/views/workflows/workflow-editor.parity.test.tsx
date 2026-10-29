// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen } from "@testing-library/react";
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
vi.mock("../common/actor-avatar", () => ({ ActorAvatar: () => null }));

const keys = ["backlog", "todo", "in_progress", "in_review", "blocked", "done", "cancelled"];

function renderPage(patch: Partial<IssueWorkflowStep>) {
  const steps: IssueWorkflowStep[] = keys.map((status_key) => ({
    status_key,
    handler: { type: "none" },
    instructions: "",
    ...(status_key === "in_progress" ? patch : {}),
  }));
  const workflow: IssueWorkflow = {
    id: "wf-1", workspace_id: "ws-1", name: "Delivery", description: "",
    is_default: false, owner_id: null, project_ids: [], steps,
    created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", version: "v1",
  };
  const statuses = keys.map((key) => ({
    id: key, workspace_id: "ws-1", key, name: key, description: "",
    category: "started", color: "#888888", is_system: true, archived_at: null,
  })) as IssueStatusEntry[];
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  client.setQueryData(issueStatusKeys.list("ws-1"), { statuses });
  client.setQueryData(projectListOptions("ws-1").queryKey, { projects: [], total: 0 });
  client.setQueryData(issueWorkflowKeys.list("ws-1"), { workflows: [workflow], total: 1 });
  renderWithI18n(
    <QueryClientProvider client={client}>
      <WorkflowEditorPage workflowId="wf-1" seed={null} canEdit onBack={() => {}} onOpen={() => {}} />
    </QueryClientProvider>,
  );
}

afterEach(cleanup);

describe("existing workflow saves", () => {
  it("permits a metadata-only edit on a workflow with legacy CI advance", () => {
    renderPage({ advance_on_ci_green: true, next_status_key: "in_review" });
    fireEvent.change(screen.getByRole("textbox", { name: "Name" }), { target: { value: "Delivery renamed" } });
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("permits a metadata-only edit on a workflow with a manual project-lead handoff", () => {
    renderPage({ handler: { type: "project_lead" } });
    fireEvent.change(screen.getByRole("textbox", { name: "Name" }), { target: { value: "Delivery renamed" } });
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("removes legacy incoming next/back references when deleting their destination column", () => {
    renderPage({
      handler: { type: "agent", id: "0f0e0d0c-0b0a-4909-8807-060504030201" },
      next_status_key: "in_review",
    });
    expect(screen.queryByRole("alert")).toBeNull();
    fireEvent.click(screen.getAllByRole("button", { pressed: false, name: /^In Review/ })[0]!);
    fireEvent.click(screen.getByRole("button", { name: "Remove column" }));
    expect(screen.queryByRole("alert")).toBeNull();
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false);
  });
});
