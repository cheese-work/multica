/**
 * @vitest-environment jsdom
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import {
  getIssueSurfaceViewStore,
  pruneIssueSurfaceViewStates,
} from "@multica/core/issues/stores/surface-view-store";
import { ViewStoreProvider } from "@multica/core/issues/stores/view-store-context";
import type { IssueStatusCategory, IssueWorkflow } from "@multica/core/types";
import { useIssueSurfaceController } from "./use-issue-surface-controller";

/** Project workflow A lists todo and done. Status qa_b exists only in override workflow B. */
const { projectWorkflowA } = vi.hoisted(() => ({
  projectWorkflowA: {
    id: "wf-a",
    workspace_id: "ws-1",
    name: "A",
    description: "",
    is_default: false,
    owner_id: null,
    steps: ["todo", "done"].map((status_key) => ({
      status_key,
      handler: { type: "none" as const },
      instructions: "",
    })),
    project_ids: ["p1"],
    created_at: "",
    updated_at: "",
  } satisfies IssueWorkflow,
}));

vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: { user: { id: string } }) => unknown) =>
    selector({ user: { id: "user-1" } }),
}));
vi.mock("@multica/core/config", () => ({ useFeatureEnabled: () => true }));
vi.mock("@multica/core/issue-workflows", () => ({
  defaultWorkflow: () => projectWorkflowA,
  useIssueWorkflows: () => ({ workflows: [projectWorkflowA] }),
  useProjectWorkflow: () => projectWorkflowA,
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/issues/mutations", () => ({
  useUpdateIssue: () => ({ mutate: vi.fn(), isPending: false }),
  useBatchUpdateIssues: () => ({ mutateAsync: vi.fn(), isPending: false }),
  useBatchDeleteIssues: () => ({ mutateAsync: vi.fn(), isPending: false }),
}));
vi.mock("@multica/core/modals", () => ({
  useModalStore: { getState: () => ({ open: vi.fn() }) },
}));
vi.mock("../../i18n", () => ({ useT: () => ({ t: () => "translated" }) }));

const QA_B = {
  id: "s-qa-b",
  workspace_id: "ws-1",
  key: "qa_b",
  name: "QA B",
  description: "",
  category: "in_progress" as IssueStatusCategory,
  color: "#ff0000",
  is_system: false,
  position: 1,
  archived_at: null,
  created_at: "",
  updated_at: "",
};

const never = <T,>() => new Promise<T>(() => {});

describe("issue surface workflow filter", () => {
  let qc: QueryClient;
  const surfaceKey = "project:p1";

  function wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={qc}>
        <ViewStoreProvider store={getIssueSurfaceViewStore(surfaceKey)}>
          {children}
        </ViewStoreProvider>
      </QueryClientProvider>
    );
  }

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    setApiInstance({
      listIssueStatuses: async () => ({ statuses: [QA_B], categories: [], total: 1 }),
      listIssues: vi.fn(() => never()),
      listIssueTableRows: vi.fn(() => never()),
      listIssueTableFacets: vi.fn(() => never()),
      listGroupedIssues: vi.fn(() => never()),
      listProjects: vi.fn(() => never()),
      getAgentTaskSnapshot: vi.fn(() => never()),
      getWorkspaceWorkingAgents: vi.fn(() => Promise.resolve([])),
      getChildIssueProgress: vi.fn(() => never()),
    } as unknown as ApiClient);
    pruneIssueSurfaceViewStates([]);
  });

  afterEach(() => {
    cleanup();
    qc.clear();
    pruneIssueSurfaceViewStates([]);
  });

  function render() {
    return renderHook(
      () =>
        useIssueSurfaceController({
          scope: { type: "project", projectId: "p1" },
          modes: ["list"],
        }),
      { wrapper },
    );
  }

  it("keeps the project's workflow columns without a workflow filter", async () => {
    const { result } = render();
    await waitFor(() => expect(result.current.columnStatuses).toEqual(["todo", "done"]));
    expect(result.current.visibleStatuses).not.toContain("qa_b");
  });

  it("includes a status only the override workflow lists once that workflow is filtered", async () => {
    act(() => getIssueSurfaceViewStore(surfaceKey).getState().toggleWorkflowFilter("wf-b"));
    const { result } = render();
    await waitFor(() => expect(result.current.visibleStatuses).toContain("qa_b"));
  });
});
