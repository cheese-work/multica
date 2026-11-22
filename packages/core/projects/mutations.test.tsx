/**
 * @vitest-environment jsdom
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import { setCurrentWorkspace } from "../platform/workspace-storage";
import {
  getIssueSurfaceViewStore,
  pruneIssueSurfaceViewStates,
} from "../issues/stores/surface-view-store";
import { issueKeys } from "../issues/queries";
import { projectKeys } from "./queries";
import type { Project } from "../types";
import { useDeleteProject, useUpdateProject } from "./mutations";

vi.mock("../hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));

function createWrapper(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

describe("useDeleteProject", () => {
  let qc: QueryClient;
  let deleteProject: ReturnType<typeof vi.fn<() => Promise<void>>>;

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    deleteProject = vi.fn().mockResolvedValue(undefined);
    setApiInstance({ deleteProject } as unknown as ApiClient);
    setCurrentWorkspace("acme", "ws-1");
  });

  afterEach(() => {
    qc.clear();
    pruneIssueSurfaceViewStates([]);
    setCurrentWorkspace(null, null);
    vi.restoreAllMocks();
  });

  it("clears the deleted project's issue surface view state", async () => {
    const store = getIssueSurfaceViewStore("project:p1");
    store.getState().setViewMode("list");
    expect(store.getState().viewMode).toBe("list");

    const { result } = renderHook(() => useDeleteProject(), {
      wrapper: createWrapper(qc),
    });

    await act(async () => {
      await result.current.mutateAsync("p1");
    });

    expect(deleteProject).toHaveBeenCalledWith("p1");
    expect(store.getState().viewMode).toBe("board");
  });

  // Regression: the issue-table invalidation once sat on the create
  // mutation, so a missed realtime event left a project-status-filtered
  // window showing the deleted project's issues (staleTime is Infinity).
  it("invalidates the issue table windows", async () => {
    const tableKey = [...issueKeys.tableAll("ws-1"), "window"];
    qc.setQueryData(tableKey, { rows: [] });

    const { result } = renderHook(() => useDeleteProject(), {
      wrapper: createWrapper(qc),
    });

    await act(async () => {
      await result.current.mutateAsync("p1");
    });

    expect(qc.getQueryState(tableKey)?.isInvalidated).toBe(true);
  });
});

describe("useUpdateProject", () => {
  let qc: QueryClient;
  let updateProject: ReturnType<typeof vi.fn>;
  const tableKey = [...issueKeys.tableAll("ws-1"), "window"];

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    updateProject = vi.fn().mockResolvedValue({ id: "p1" });
    setApiInstance({ updateProject } as unknown as ApiClient);
    qc.setQueryData(tableKey, { rows: [] });
  });

  afterEach(() => {
    qc.clear();
    vi.restoreAllMocks();
  });

  it("invalidates the issue table windows when the status changes", async () => {
    const { result } = renderHook(() => useUpdateProject(), {
      wrapper: createWrapper(qc),
    });

    await act(async () => {
      await result.current.mutateAsync({ id: "p1", status: "paused" });
    });

    expect(qc.getQueryState(tableKey)?.isInvalidated).toBe(true);
  });

  it("leaves the issue table windows alone when the status is untouched", async () => {
    const { result } = renderHook(() => useUpdateProject(), {
      wrapper: createWrapper(qc),
    });

    await act(async () => {
      await result.current.mutateAsync({ id: "p1", title: "Renamed" });
    });

    expect(qc.getQueryState(tableKey)?.isInvalidated).toBe(false);
  });

  // CHE-7654: a status change cancels runs and settles wakeups on the
  // server, so it is not a predictable patch. The cache must keep the old
  // status until the server answers.
  describe("optimistic patching", () => {
    const base = { id: "p1", title: "P", status: "planned", priority: "none" } as unknown as Project;
    const seed = () => {
      qc.setQueryData(projectKeys.detail("ws-1", "p1"), base);
      qc.setQueryData(projectKeys.list("ws-1"), { projects: [base], total: 1 });
    };
    const detail = () => qc.getQueryData<Project>(projectKeys.detail("ws-1", "p1"));
    const list = () =>
      qc.getQueryData<{ projects: Project[] }>(projectKeys.list("ws-1"))?.projects[0];

    it("does not patch the cache for a status change while the request runs", async () => {
      let resolve!: (v: unknown) => void;
      updateProject.mockReturnValue(new Promise((r) => (resolve = r)));
      seed();
      const { result } = renderHook(() => useUpdateProject(), { wrapper: createWrapper(qc) });

      act(() => {
        result.current.mutate({ id: "p1", status: "paused" });
      });
      await vi.waitFor(() => expect(result.current.isPending).toBe(true));

      expect(detail()?.status).toBe("planned");
      expect(list()?.status).toBe("planned");
      await act(async () => resolve({ ...base, status: "paused" }));
    });

    it("keeps the old status when the status change is rejected", async () => {
      updateProject.mockRejectedValue(new Error("boom"));
      seed();
      const { result } = renderHook(() => useUpdateProject(), { wrapper: createWrapper(qc) });

      await act(async () => {
        await result.current.mutateAsync({ id: "p1", status: "paused" }).catch(() => {});
      });

      expect(detail()?.status).toBe("planned");
      expect(list()?.status).toBe("planned");
    });

    it("still patches other fields optimistically", async () => {
      updateProject.mockReturnValue(new Promise(() => {}));
      seed();
      const { result } = renderHook(() => useUpdateProject(), { wrapper: createWrapper(qc) });

      act(() => {
        result.current.mutate({ id: "p1", priority: "high" });
      });
      await vi.waitFor(() => expect(detail()?.priority).toBe("high"));
      expect(list()?.priority).toBe("high");
    });
  });
});
