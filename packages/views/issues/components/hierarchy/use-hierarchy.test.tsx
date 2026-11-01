import type { ReactNode } from "react";
import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Issue, IssueRelations } from "@multica/core/types";
import { useHierarchy } from "./use-hierarchy";

const api = vi.hoisted(() => ({ getIssue: vi.fn(), listChildIssues: vi.fn(), getIssueRelations: vi.fn() }));
vi.mock("@multica/core/api", () => ({ api }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/issue-statuses/hooks", () => ({ useIssueStatuses: () => ({ categoryOf: () => "unstarted" }) }));

const root = { id: "root", parent_issue_id: null, identifier: "CHE-1", title: "Root", status: "todo" };
const node = (n: number) => ({ id: `id-${n}`, identifier: `CHE-${n}`, title: `Issue ${n}`, status: "todo" });
const graph = (wakes: number, omitted = 0): IssueRelations => ({
  nodes: [node(0), ...Array.from({ length: wakes }, (_, i) => node(i + 1))],
  edges: Array.from({ length: wakes }, (_, i) => ({ source: "root", target: `id-${i + 1}`, type: "wakeup" as const })),
  omitted_count: omitted,
  depth: 1,
});

function mount(wakes = false, mentions = false) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  return renderHook(() => useHierarchy(root as unknown as Issue, { wakes, mentions }), { wrapper });
}

beforeEach(() => {
  vi.resetAllMocks();
  api.listChildIssues.mockResolvedValue({ issues: [] });
  api.getIssueRelations.mockResolvedValue(graph(0));
});

describe("useHierarchy", () => {
  it("recovers on retry when a root issue's children request failed first", async () => {
    api.listChildIssues.mockRejectedValueOnce(new Error("boom"));
    const { result } = mount();
    await waitFor(() => expect(result.current.error).toBe(true));
    act(() => result.current.refetch());
    await waitFor(() => expect(result.current.hierarchy).not.toBeNull());
    expect(result.current.error).toBe(false);
    expect(api.getIssue).not.toHaveBeenCalled();
  });

  it("counts wakes exactly when the relations response is complete", async () => {
    api.getIssueRelations.mockResolvedValue(graph(3));
    const { result } = mount();
    await waitFor(() => expect(result.current.wakeCount).toEqual({ value: 3, partial: false }));
  });

  it("marks the wake count partial when the relations response was truncated", async () => {
    api.getIssueRelations.mockResolvedValue(graph(40, 12));
    const { result } = mount();
    await waitFor(() => expect(result.current.wakeCount).toEqual({ value: 40, partial: true }));
  });

  it("reports an unknown wake count, not zero, when the relations request failed", async () => {
    api.getIssueRelations.mockRejectedValue(new Error("boom"));
    const { result } = mount();
    await waitFor(() => expect(result.current.hierarchy).not.toBeNull());
    await waitFor(() => expect(result.current.wakeCount).toEqual({ value: null, partial: false }));
  });

  it("marks the mention count partial when truncated and unknown when failed", async () => {
    api.getIssueRelations.mockImplementation((_ws: string, _id: string, _depth: number, _signal: unknown, mentions: boolean) =>
      mentions ? Promise.resolve({ ...graph(0, 5), edges: [{ source: "root", target: "id-1", type: "mention" }], nodes: [node(0), node(1)] }) : Promise.resolve(graph(0)));
    const { result } = mount(false, true);
    await waitFor(() => expect(result.current.mentionCount).toEqual({ value: 1, partial: true }));

    api.getIssueRelations.mockImplementation((_ws: string, _id: string, _depth: number, _signal: unknown, mentions: boolean) =>
      mentions ? Promise.reject(new Error("boom")) : Promise.resolve(graph(0)));
    const failed = mount(false, true);
    await waitFor(() => expect(failed.result.current.mentionCount).toEqual({ value: null, partial: false }));
  });
});
