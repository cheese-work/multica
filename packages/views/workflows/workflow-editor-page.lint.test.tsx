/** @vitest-environment jsdom */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiError, api } from "@multica/core/api";
import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import { setCurrentWorkspace } from "@multica/core/platform";
import { issueStatusKeys } from "@multica/core/issue-statuses/queries";
import { issueWorkflowKeys } from "@multica/core/issue-workflows";
import { projectListOptions } from "@multica/core/projects/queries";
import type { IssueStatusEntry, IssueWorkflow, IssueWorkflowLintFinding, WorkflowLintResponse } from "@multica/core/types";
import { renderWithI18n } from "../test/i18n";
import { WorkflowEditorPage } from "./workflow-editor-page";

vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: { updateIssueWorkflow: vi.fn(), createIssueWorkflow: vi.fn(), listIssueWorkflows: vi.fn(), lintIssueWorkflow: vi.fn() },
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

const finding = (over: Partial<IssueWorkflowLintFinding>): IssueWorkflowLintFinding => ({
  rule: "WF003", level: "error", step: "in_progress",
  message: "in_progress has no exit.", hint: "Add an edge to done.", waived: false, waive_reason: "", ...over,
});
const findings = [
  finding({}),
  finding({ rule: "WF010", level: "warning", step: "todo", message: "todo hands off to nobody.", hint: "Pick a handler." }),
];
const lint: WorkflowLintResponse = { workflow_id: "wf-1", workflow: "Delivery", findings, errors: 1, warnings: 1, infos: 0 };

function refusal() {
  return new ApiError("save refused", 422, "Unprocessable Entity", {
    code: "workflow_lint_errors",
    error: "save refused",
    findings: [findings[0]],
  });
}

function mount(response: WorkflowLintResponse = lint) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  qc.setQueryData(issueStatusKeys.list("ws-1"), { statuses });
  qc.setQueryData(projectListOptions("ws-1").queryKey, { projects: [], total: 0 });
  qc.setQueryData(issueWorkflowKeys.list("ws-1"), { workflows: [saved], total: 1 });
  qc.setQueryData(issueWorkflowKeys.lint("ws-1", "wf-1"), response);
  return renderWithI18n(
    <QueryClientProvider client={qc}>
      <WorkflowEditorPage workflowId="wf-1" seed={null} canEdit onBack={() => {}} onOpen={() => {}} />
    </QueryClientProvider>,
  );
}

const rename = (to: string) => fireEvent.change(screen.getByDisplayValue(/^Delivery/), { target: { value: to } });

beforeEach(async () => {
  localStorage.clear();
  resetAllRegisteredDrafts();
  vi.mocked(api.updateIssueWorkflow).mockReset();
  setCurrentWorkspace("acme", "ws-1");
  await new Promise((r) => queueMicrotask(() => r(null)));
});
afterEach(cleanup);

describe("WorkflowEditorPage lint", () => {
  it("counts the findings on the Checks button", () => {
    mount();
    const checks = screen.getByRole("button", { name: /^Checks/ });
    expect(within(checks).getByLabelText("1 error")).toBeTruthy();
    expect(within(checks).getByLabelText("1 warning")).toBeTruthy();
  });

  it("marks the step cards that have findings", () => {
    mount();
    expect(screen.getByRole("button", { name: /^In Progress.*1 error/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /^To ?do.*1 warning/i })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Done.*(error|warning)/ })).toBeNull();
  });

  it("opens the Checks sheet and lists the findings", () => {
    mount();
    fireEvent.click(screen.getByRole("button", { name: /^Checks/ }));
    expect(screen.getByText("in_progress has no exit.")).toBeTruthy();
    expect(screen.getByText("Add an edge to done.")).toBeTruthy();
  });

  it("opens the refusal dialog on a 422 and sends the reason on Save anyway", async () => {
    vi.mocked(api.updateIssueWorkflow)
      .mockRejectedValueOnce(refusal())
      .mockResolvedValueOnce({ ...saved, name: "Delivery v2", version: "v2" } as IssueWorkflow);
    mount();
    rename("Delivery v2");
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("dialog")).toBeTruthy();
    expect(screen.getByText("in_progress has no exit.")).toBeTruthy();

    fireEvent.change(screen.getByRole("textbox", { name: "Reason for saving with errors" }), { target: { value: "Exit comes next sprint" } });
    fireEvent.click(screen.getByRole("button", { name: "Save anyway" }));
    await waitFor(() => expect(api.updateIssueWorkflow).toHaveBeenCalledTimes(2));
    expect(vi.mocked(api.updateIssueWorkflow).mock.calls[0]![1]).not.toHaveProperty("lint_override");
    expect(vi.mocked(api.updateIssueWorkflow).mock.calls[1]![1]).toMatchObject({ lint_override: "Exit comes next sprint" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("keeps the typed reason across a remount, and clears it after a saved override", async () => {
    vi.mocked(api.updateIssueWorkflow).mockRejectedValueOnce(refusal());
    const first = mount();
    rename("Delivery v2");
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    fireEvent.change(await screen.findByRole("textbox", { name: "Reason for saving with errors" }), { target: { value: "Half-typed reason" } });
    first.unmount();

    vi.mocked(api.updateIssueWorkflow)
      .mockRejectedValueOnce(refusal())
      .mockResolvedValueOnce({ ...saved, name: "Delivery v2", version: "v2" } as IssueWorkflow)
      .mockRejectedValueOnce(refusal());
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Save" }));
    const reason = (await screen.findByRole("textbox", { name: "Reason for saving with errors" })) as HTMLTextAreaElement;
    expect(reason.value).toBe("Half-typed reason");

    fireEvent.click(screen.getByRole("button", { name: "Save anyway" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    rename("Delivery v3");
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    const again = (await screen.findByRole("textbox", { name: "Reason for saving with errors" })) as HTMLTextAreaElement;
    expect(again.value).toBe("");
  });

  it("falls back to the toast path on a refusal with no findings", async () => {
    vi.mocked(api.updateIssueWorkflow).mockRejectedValueOnce(
      new ApiError("save refused", 422, "Unprocessable Entity", { code: "workflow_lint_errors", findings: "nope" }),
    );
    mount();
    rename("Delivery v2");
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.updateIssueWorkflow).toHaveBeenCalled());
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
