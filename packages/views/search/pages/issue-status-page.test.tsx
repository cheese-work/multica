import type { ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { Command as CommandPrimitive } from "cmdk";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import { projectKeys } from "@multica/core/projects/queries";
import type { Issue } from "@multica/core/types";
import { RESOURCES } from "../../locales";
import { PaletteContext } from "../palette-context";
import type { IssueTarget } from "../types";
import { IssueStatusPage } from "./issue-pages";

const { mockApply, mockToast, mockStatusOptions, mockListProjects, workflows } = vi.hoisted(() => {
  const step = (status_key: string) => ({ status_key, handler: { type: "none" } });
  const wf = (id: string, keys: string[]) => ({ id, name: id, is_default: false, steps: keys.map(step) });
  return {
    mockApply: vi.fn(),
    mockToast: Object.assign(vi.fn(), { success: vi.fn(), error: vi.fn(), warning: vi.fn() }),
    mockStatusOptions: vi.fn(),
    mockListProjects: vi.fn(),
    workflows: [wf("wide", ["todo", "in_progress", "done"]), wf("narrow", ["todo", "done"])],
  };
});

vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: { listProjects: mockListProjects },
}));
vi.mock("sonner", () => ({ toast: mockToast }));
vi.mock("@multica/core/hooks", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/hooks")>()),
  useWorkspaceId: () => "ws-test",
}));
vi.mock("@multica/core/issue-workflows", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/issue-workflows")>()),
  useIssueWorkflows: () => ({ workflows, isLoaded: true }),
}));
vi.mock("../../issues/utils/status-options", () => ({ useIssueStatusOptions: mockStatusOptions }));
vi.mock("../issue-target", () => ({
  targetIssues: () => [],
  useIssueTargetWriter: (target: IssueTarget) => ({
    issues: target.kind === "single" ? [target.issue] : target.issues,
    apply: mockApply,
  }),
}));

const options = ["todo", "in_progress", "done"].map((key) => ({ key, category: key, label: key, color: null }));
const issue = (id: string, extra: Partial<Issue> = {}) =>
  ({ id, identifier: id, status: "todo", project_id: "p1", workflow_id: null, ...extra }) as unknown as Issue;

function renderPage(target: IssueTarget, projectWorkflow: string | null = "wide") {
  const client = new QueryClient();
  if (projectWorkflow) {
    client.setQueryData(projectKeys.list("ws-test"), { projects: [{ id: "p1", workflow_id: projectWorkflow }] });
  }
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <I18nProvider locale="en" resources={{ en: RESOURCES.en }}>
        <PaletteContext.Provider value={{ close: vi.fn() } as never}>
          <CommandPrimitive>{children}</CommandPrimitive>
        </PaletteContext.Provider>
      </I18nProvider>
    </QueryClientProvider>
  );
  return render(<IssueStatusPage target={target} query="" />, { wrapper });
}

describe("IssueStatusPage workflow override (CHE-2822)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockStatusOptions.mockReturnValue({ options, workflow: null, resolved: true, retry: vi.fn() });
  });

  it("holds a single-issue write while its override is unresolved", () => {
    mockStatusOptions.mockReturnValue({ options, workflow: null, resolved: false, retry: vi.fn() });
    renderPage({ kind: "single", issue: issue("A", { project_id: null, workflow_id: "gone" }) });
    fireEvent.click(screen.getByText("done"));
    expect(mockApply).not.toHaveBeenCalled();
    expect(mockToast.error).toHaveBeenCalled();
  });

  it("offers only the statuses every selected workflow allows when overrides are mixed", () => {
    renderPage({
      kind: "batch",
      issues: [issue("A"), issue("B", { workflow_id: "narrow" })],
      clearSelection: vi.fn(),
    });
    expect(screen.queryByText("in_progress")).toBeNull();
    expect(screen.getByText("todo")).toBeTruthy();
    expect(screen.getByText("done")).toBeTruthy();
  });

  const mixed = (): IssueTarget => ({
    kind: "batch",
    issues: [issue("A"), issue("B", { workflow_id: "wide" })],
    clearSelection: vi.fn(),
  });

  it("offers nothing while the projects of a same-project override batch are loading", () => {
    mockListProjects.mockReturnValue(new Promise(() => {}));
    renderPage(mixed(), null);
    expect(screen.queryByText("todo")).toBeNull();
    expect(screen.queryByText("in_progress")).toBeNull();
  });

  it("loads the projects and applies the project workflow to issues without an override", async () => {
    mockListProjects.mockResolvedValue({ projects: [{ id: "p1", workflow_id: "narrow" }] });
    renderPage(mixed(), null);
    await waitFor(() => expect(screen.getByText("todo")).toBeTruthy());
    expect(screen.queryByText("in_progress")).toBeNull();
  });
});
