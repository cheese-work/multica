import { QueryClient } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import { buildGraph } from "./graph-core";
import {
  fetchRelationsBatched,
  invalidateIssueGraph,
  issueActualTimelineOptions,
  issueGraphKeys,
  mergeTimelinePages,
  RELATIONS_BATCH_CHUNK,
} from "./graph-queries";
import { api } from "../api";
import type { RelationsBatchResponse, IssueTimelineResponse } from "../api/issue-graph-schema";

vi.mock("../api", () => ({ api: { getIssueRelationsBatch: vi.fn(), getIssueActualTimeline: vi.fn() } }));
const mocked = api as unknown as { getIssueRelationsBatch: ReturnType<typeof vi.fn>; getIssueActualTimeline: ReturnType<typeof vi.fn> };
afterEach(() => vi.clearAllMocks());

const id = (n: number) => `00000000-0000-4000-8000-${String(n).padStart(12, "0")}`;
const node = (n: number, context = false) => ({ id: id(n), identifier: `CHE-${n}`, title: `t${n}`, status: "todo", parent_issue_id: null, context });
const edge = (a: number, b: number, type = "dependency" as const) => ({ source: id(a), target: id(b), type });
const page = (nodes: ReturnType<typeof node>[], edges: ReturnType<typeof edge>[], next_cursor: string | null = null): RelationsBatchResponse => ({ nodes, edges, next_cursor });

describe("fetchRelationsBatched", () => {
  it("X3: an edge between two 200-id chunks shows once after the merge, and context yields to in_response", async () => {
    const ids = Array.from({ length: RELATIONS_BATCH_CHUNK + 1 }, (_, i) => id(i + 1));
    const first = id(1);
    const last = id(RELATIONS_BATCH_CHUNK + 1);
    mocked.getIssueRelationsBatch.mockImplementation(async (_ws: string, body: { issue_ids: string[] }) => {
      const inFirstChunk = body.issue_ids.includes(first);
      // Both chunks return the same cross-chunk edge; each sees the other end as context.
      return inFirstChunk
        ? page([node(1), node(RELATIONS_BATCH_CHUNK + 1, true)], [{ source: first, target: last, type: "dependency" }])
        : page([node(RELATIONS_BATCH_CHUNK + 1), node(1, true)], [{ source: first, target: last, type: "dependency" }]);
    });
    const out = await fetchRelationsBatched("ws", ids, { types: ["dependency"] });
    expect(mocked.getIssueRelationsBatch).toHaveBeenCalledTimes(2);
    expect(mocked.getIssueRelationsBatch.mock.calls.map((c) => c[1].issue_ids.length)).toEqual([200, 1]);
    expect(out.edges).toHaveLength(1);
    expect(out.nodes).toHaveLength(2);
    expect(out.nodes.every((n) => !n.context)).toBe(true);
    expect(out.incomplete).toBe(false);
    expect(out.notLoaded).toEqual([]);
  });

  it("follows next_cursor until the edge set is complete and drops repeated edges", async () => {
    mocked.getIssueRelationsBatch
      .mockResolvedValueOnce(page([node(1), node(2, true)], [edge(1, 2)], "c1"))
      .mockResolvedValueOnce(page([node(1), node(3, true)], [edge(1, 2), edge(1, 3)], null));
    const out = await fetchRelationsBatched("ws", [id(1)], {});
    expect(mocked.getIssueRelationsBatch.mock.calls[1]![1].page.cursor).toBe("c1");
    expect(out.edges).toHaveLength(2);
    expect(out.nodes.map((n) => n.id).sort()).toEqual([id(1), id(2), id(3)]);
    expect(out.nodes.find((n) => n.id === id(2))!.context).toBe(true);
  });

  it("X5: a failed second page marks the chunk's ids not_loaded and keeps the pages that loaded; a retry completes", async () => {
    mocked.getIssueRelationsBatch
      .mockResolvedValueOnce(page([node(1), node(2)], [edge(1, 2)], "c1"))
      .mockRejectedValueOnce(new Error("boom"));
    const out = await fetchRelationsBatched("ws", [id(1), id(2)], {});
    expect(out.incomplete).toBe(true);
    expect(out.notLoaded.sort()).toEqual([id(1), id(2)]);
    expect(out.edges).toHaveLength(1);
    // K1: no retarget and no "target inside" for a not_loaded endpoint.
    const view = buildGraph({
      nodes: [{ id: id(1), identifier: "CHE-1", title: "", status: "todo", parentId: null }, { id: id(2), identifier: "CHE-2", title: "", status: "todo", parentId: null }],
      edges: out.edges, notLoaded: new Set(out.notLoaded),
    });
    expect(view.edges[0]).toMatchObject({ sourceState: "not_loaded", targetState: "not_loaded", sourceInside: false, targetInside: false });

    mocked.getIssueRelationsBatch
      .mockResolvedValueOnce(page([node(1), node(2)], [edge(1, 2)], "c1"))
      .mockResolvedValueOnce(page([node(1), node(2)], [edge(1, 2, "parent" as never)], null));
    const retry = await fetchRelationsBatched("ws", [id(1), id(2)], {});
    expect(retry.incomplete).toBe(false);
    expect(retry.notLoaded).toEqual([]);
    expect(retry.edges).toHaveLength(2);
  });

  it("onProgress gets the merged result after each page, and a failed page shows as incomplete", async () => {
    mocked.getIssueRelationsBatch
      .mockResolvedValueOnce(page([node(1)], [edge(1, 2)], "c1"))
      .mockResolvedValueOnce(page([node(2)], [edge(2, 3)], "c2"))
      .mockRejectedValueOnce(new Error("boom"));
    const seen: [number, number, boolean][] = [];
    const out = await fetchRelationsBatched("ws", [id(1)], {}, undefined, (p) => seen.push([p.nodes.length, p.edges.length, p.incomplete]));
    expect(seen).toEqual([[1, 1, false], [2, 2, false]]);
    expect(out).toMatchObject({ incomplete: true, notLoaded: [id(1)] });
  });

  it("rethrows cancellation instead of reporting it as an incomplete graph", async () => {
    const abort = new DOMException("aborted", "AbortError");
    mocked.getIssueRelationsBatch.mockRejectedValue(abort);
    await expect(fetchRelationsBatched("ws", [id(1)], {})).rejects.toBe(abort);
  });

  it("returns an empty complete graph for no ids without a request", async () => {
    const out = await fetchRelationsBatched("ws", [], {});
    expect(mocked.getIssueRelationsBatch).not.toHaveBeenCalled();
    expect(out).toEqual({ nodes: [], edges: [], incomplete: false, notLoaded: [] });
  });
});

describe("mergeTimelinePages", () => {
  const tl = (issues: IssueTimelineResponse["issues"], wakeup_events: IssueTimelineResponse["wakeup_events"] = []): IssueTimelineResponse => ({
    as_of: "2026-11-13T00:00:00Z", window: { from: "a", to: "b" }, issues, wakeup_events, next_cursor: null,
  });
  const issue = (n: number, segs: { row_id: string; to?: string | null }[]) => ({
    issue_id: id(n), status: "in_progress", history: "complete" as const, start_known: true,
    actual_start: null, actual_end: null, presence_from: null, presence_to: null, start_date: null, due_date: null,
    segments: segs.map((s) => ({ row_id: s.row_id, status: "in_progress", category: "started" as const, from: "2026-11-01T00:00:00Z", to: s.to ?? null, clipped_start: false, clipped_end: false })),
  });

  it("merges by issue id and segment row id so a segment never shows twice (older-window load)", () => {
    const merged = mergeTimelinePages([
      tl([issue(1, [{ row_id: "r2" }])]),
      tl([issue(1, [{ row_id: "r1" }, { row_id: "r2" }]), issue(2, [{ row_id: "r9" }])]),
    ]);
    expect(merged.issues.map((i) => i.issue_id)).toEqual([id(1), id(2)]);
    expect(merged.issues[0]!.segments.map((s) => s.row_id)).toEqual(["r2", "r1"]);
  });

  it("unions the pieces of one segment that the server clipped at the window boundary", () => {
    const seg = (from: string | null, to: string | null, clipped_start: boolean, clipped_end: boolean) =>
      ({ row_id: "r1", status: "in_progress", category: "started" as const, from, to, clipped_start, clipped_end });
    const mk = (n: number, segments: ReturnType<typeof seg>[]) => ({ ...issue(n, []), segments });
    // Current window sees the tail; the older window sees the head of the same row.
    const merged = mergeTimelinePages([
      tl([mk(1, [seg("2026-11-01T00:00:00Z", "2026-11-10T00:00:00Z", true, false)])]),
      tl([mk(1, [seg("2026-10-01T00:00:00Z", "2026-11-01T00:00:00Z", false, true)])]),
    ]);
    expect(merged.issues[0]!.segments).toEqual([seg("2026-10-01T00:00:00Z", "2026-11-10T00:00:00Z", false, false)]);
  });

  it("keeps an open end open when one piece has no end", () => {
    const seg = (from: string, to: string | null, clipped_end: boolean) =>
      ({ row_id: "r1", status: "in_progress", category: "started" as const, from, to, clipped_start: false, clipped_end });
    const mk = (segments: ReturnType<typeof seg>[]) => ({ ...issue(1, []), segments });
    const merged = mergeTimelinePages([
      tl([mk([seg("2026-11-01T00:00:00Z", null, false)])]),
      tl([mk([seg("2026-10-01T00:00:00Z", "2026-11-01T00:00:00Z", true)])]),
    ]);
    expect(merged.issues[0]!.segments[0]).toMatchObject({ from: "2026-10-01T00:00:00Z", to: null, clipped_end: false });
  });

  it("merges wakeup events by id", () => {
    const ev = (eid: string) => ({ id: eid, at: "x", target_issue_id: id(1), rule_id: "r", rule_kind: "at", condition_type: "", source_issue_id: null, class: "self" as const, run_id: null });
    const merged = mergeTimelinePages([tl([], [ev("e1")]), tl([], [ev("e1"), ev("e2")])]);
    expect(merged.wakeup_events.map((e) => e.id)).toEqual(["e1", "e2"]);
  });
});

describe("timeline query options and invalidation", () => {
  const request = { scope: { kind: "workspace" as const }, filters: {} };

  it("keys are scoped by workspace and include scope, filters and window", () => {
    const a = issueActualTimelineOptions("ws-1", request).queryKey;
    const b = issueActualTimelineOptions("ws-2", request).queryKey;
    const c = issueActualTimelineOptions("ws-1", { ...request, window: { from: "f", to: "t" } }).queryKey;
    expect(a[1]).toBe("ws-1");
    expect(b[1]).toBe("ws-2");
    expect(JSON.stringify(a)).not.toEqual(JSON.stringify(c));
    expect(a.slice(0, 3)).toEqual(issueGraphKeys.all("ws-1"));
  });

  it("pages with the server cursor", async () => {
    const options = issueActualTimelineOptions("ws-1", request);
    mocked.getIssueActualTimeline.mockResolvedValue({ next_cursor: "n" });
    await (options.queryFn as (c: unknown) => Promise<unknown>)({ pageParam: "cur", signal: undefined });
    expect(mocked.getIssueActualTimeline.mock.calls[0]![1].page.cursor).toBe("cur");
    const last = { next_cursor: "n" } as IssueTimelineResponse;
    expect(options.getNextPageParam!(last, [], null, [])).toBe("n");
    expect(options.getNextPageParam!({ next_cursor: null } as IssueTimelineResponse, [], null, [])).toBeUndefined();
  });

  it("invalidateIssueGraph marks timeline and relation queries of that workspace stale, not another workspace", () => {
    const qc = new QueryClient();
    const mine = [...issueGraphKeys.all("ws-1"), "timeline", {}] as const;
    const rel = issueGraphKeys.relations("ws-1", [id(1)], {});
    const other = [...issueGraphKeys.all("ws-2"), "timeline", {}] as const;
    for (const key of [mine, rel, other]) qc.setQueryData(key, {});
    invalidateIssueGraph(qc, "ws-1");
    expect(qc.getQueryState(mine)?.isInvalidated).toBe(true);
    expect(qc.getQueryState(rel)?.isInvalidated).toBe(true);
    expect(qc.getQueryState(other)?.isInvalidated).toBe(false);
  });
});
