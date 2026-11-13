import { act, renderHook, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { capLevel, useHierarchyLevels } from "./use-hierarchy-levels";

const api = vi.hoisted(() => ({ getIssueRelationsBatch: vi.fn() }));
vi.mock("@multica/core/api", () => ({ api }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));

const id = (n: number) => `00000000-0000-4000-8000-${String(n).padStart(12, "0")}`;
const ROOT = id(0);
const issue = (n: number, parent: number | null = 0) => ({ id: id(n), identifier: `CHE-${n}`, title: `t${n}`, status: "todo", parent_issue_id: parent === null ? null : id(parent), context: false });
const kids = (from: number, to: number) => Array.from({ length: to - from + 1 }, (_, i) => issue(from + i));
const page = (children: ReturnType<typeof issue>[], next: string | null = null) => ({
  nodes: [issue(0, null), ...children],
  edges: children.map((c) => ({ source: ROOT, target: c.id, type: "parent" as const })),
  next_cursor: next,
});
const deferred = <T,>() => {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
};

beforeEach(() => vi.resetAllMocks());

describe("useHierarchyLevels", () => {
  it("X6: 75 children load in full; 60 cards and +15; while paging the badge is pending", async () => {
    const second = deferred<ReturnType<typeof page>>();
    api.getIssueRelationsBatch
      .mockResolvedValueOnce(page(kids(1, 70), "c1"))
      .mockReturnValueOnce(second.promise);
    const { result } = renderHook(() => useHierarchyLevels(ROOT));
    act(() => result.current.expand(0));

    await waitFor(() => expect(result.current.levels[1]?.nodes).toHaveLength(70));
    expect(result.current.levels[1]!.status).toBe("loading");
    expect(capLevel(result.current.levels[1])).toMatchObject({ hidden: 10, pending: true });
    expect(capLevel(result.current.levels[1]).shown).toHaveLength(60);

    await act(async () => second.resolve(page(kids(71, 75))));
    expect(result.current.levels[1]!.status).toBe("ready");
    expect(result.current.levels[1]!.nodes).toHaveLength(75);
    expect(capLevel(result.current.levels[1])).toMatchObject({ hidden: 15, pending: false });
    expect(api.getIssueRelationsBatch.mock.calls[0]![1]).toMatchObject({ issue_ids: [ROOT], types: ["parent"] });
  });

  it("a failed level reports failed, keeps no final count, and a retry completes it", async () => {
    api.getIssueRelationsBatch
      .mockResolvedValueOnce(page(kids(1, 3), "c1"))
      .mockRejectedValueOnce(new Error("boom"))
      .mockResolvedValueOnce(page(kids(1, 5)));
    const { result } = renderHook(() => useHierarchyLevels(ROOT));
    act(() => result.current.expand(0));
    await waitFor(() => expect(result.current.levels[1]?.status).toBe("failed"));

    act(() => result.current.expand(0));
    await waitFor(() => expect(result.current.levels[1]?.status).toBe("ready"));
    expect(result.current.levels[1]!.nodes).toHaveLength(5);
  });

  it("an older response never replaces a newer one", async () => {
    const older = deferred<ReturnType<typeof page>>();
    api.getIssueRelationsBatch
      .mockReturnValueOnce(older.promise)
      .mockResolvedValueOnce(page(kids(1, 2)));
    const { result } = renderHook(() => useHierarchyLevels(ROOT));
    act(() => result.current.expand(0));
    act(() => result.current.expand(0));
    await waitFor(() => expect(result.current.levels[1]?.status).toBe("ready"));

    await act(async () => older.resolve(page(kids(1, 1))));
    expect(result.current.levels[1]!.nodes.map((n) => n.id)).toEqual([id(1), id(2)]);
  });

  it("a response for a previous root is dropped after the root changes", async () => {
    const older = deferred<ReturnType<typeof page>>();
    api.getIssueRelationsBatch.mockReturnValueOnce(older.promise);
    const { result, rerender } = renderHook(({ root }) => useHierarchyLevels(root), { initialProps: { root: ROOT } });
    act(() => result.current.expand(0));
    rerender({ root: id(99) });

    await act(async () => older.resolve(page(kids(1, 2))));
    expect(result.current.levels).toEqual({});
  });
});
