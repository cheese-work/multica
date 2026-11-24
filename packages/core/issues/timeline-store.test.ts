// @vitest-environment node
import { QueryClient } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { TimelineEntry } from "../types";
import type { EntityTable } from "../normalized/entity-table";

const listTimeline = vi.fn();
vi.mock("../api", () => ({ api: { listTimeline }, ApiError: class ApiError extends Error {} }));

const { issueKeys, issueTimelineOptions } = await import("./queries");
const { ingestTimeline, readTimeline, restoreTimeline, updateTimeline } = await import("./timeline-store");

const ISSUE = "issue-1";
const OTHER = "issue-2";

const entry = (id: string, over: Partial<TimelineEntry> = {}): TimelineEntry => ({
  type: "comment",
  id,
  actor_type: "member",
  actor_id: "u1",
  content: id,
  created_at: `2026-11-24T00:00:0${id.length % 10}Z`,
  ...over,
});

const newClient = () => new QueryClient({ defaultOptions: { queries: { retry: false } } });
const ids = (qc: QueryClient, issueId = ISSUE) => qc.getQueryData<string[]>(issueKeys.timeline(issueId));
const table = (qc: QueryClient, issueId = ISSUE) =>
  qc.getQueryData<EntityTable<TimelineEntry>>(issueKeys.timelineEntities(issueId));

describe("timeline store", () => {
  let qc: QueryClient;
  beforeEach(() => {
    qc = newClient();
    listTimeline.mockReset();
  });

  it("fetch stores one by-id entry per row and an ordered id list", async () => {
    const a = entry("a");
    const b = entry("bb");
    listTimeline.mockResolvedValue([a, b]);

    const result = await qc.fetchQuery(issueTimelineOptions(ISSUE));

    expect(result).toEqual(["a", "bb"]);
    expect(ids(qc)).toEqual(["a", "bb"]);
    expect(table(qc)?.byId).toEqual({ a, bb: b });
    expect(readTimeline(qc, ISSUE)).toEqual([a, b]);
  });

  it("refetch replaces row fields and prunes rows the server dropped", async () => {
    const a = entry("a", { resolved_at: "2026-11-24T01:00:00Z" });
    const b = entry("bb");
    ingestTimeline(qc, ISSUE, [a, b]);

    const next = entry("a"); // resolved_at omitted by the server
    ingestTimeline(qc, ISSUE, [next]);

    expect(ids(qc)).toEqual(["a"]);
    expect(table(qc)?.byId).toEqual({ a: next });
    expect(table(qc)?.byId.a).not.toHaveProperty("resolved_at");
  });

  it("upsert appends a new id and keeps the order the writer chose", () => {
    const a = entry("a");
    ingestTimeline(qc, ISSUE, [a]);
    const c = entry("ccc");

    updateTimeline(qc, ISSUE, (rows) => [...(rows ?? []), c]);

    expect(ids(qc)).toEqual(["a", "ccc"]);
    expect(table(qc)?.byId.ccc).toEqual(c);
    expect(table(qc)?.byId.a).toBe(a);
  });

  it("creates the store when the issue was never fetched", () => {
    const a = entry("a");
    updateTimeline(qc, ISSUE, (rows) => [...(rows ?? []), a]);
    expect(readTimeline(qc, ISSUE)).toEqual([a]);
  });

  it("a patch replaces one row and keeps every other reference", () => {
    const a = entry("a");
    const b = entry("bb");
    ingestTimeline(qc, ISSUE, [a, b]);
    const idsBefore = ids(qc);

    updateTimeline(qc, ISSUE, (rows) => rows?.map((r) => (r.id === "a" ? { ...r, content: "edited" } : r)));

    expect(table(qc)?.byId.a?.content).toBe("edited");
    expect(table(qc)?.byId.bb).toBe(b);
    expect(ids(qc)).toBe(idsBefore);
  });

  it("delete removes the id and its entry", () => {
    ingestTimeline(qc, ISSUE, [entry("a"), entry("bb")]);

    updateTimeline(qc, ISSUE, (rows) => rows?.filter((r) => r.id !== "a"));

    expect(ids(qc)).toEqual(["bb"]);
    expect(table(qc)?.byId).not.toHaveProperty("a");
  });

  it("a no-op change leaves both cache entries untouched", () => {
    ingestTimeline(qc, ISSUE, [entry("a")]);
    const idsBefore = ids(qc);
    const tableBefore = table(qc);

    updateTimeline(qc, ISSUE, (rows) => rows);
    updateTimeline(qc, ISSUE, (rows) => rows?.map((r) => r));

    expect(ids(qc)).toBe(idsBefore);
    expect(table(qc)).toBe(tableBefore);
  });

  it("optimistic patch rolls back to the snapshot", () => {
    const a = entry("a");
    ingestTimeline(qc, ISSUE, [a]);
    const prev = readTimeline(qc, ISSUE);

    updateTimeline(qc, ISSUE, (rows) => rows?.map((r) => ({ ...r, content: "optimistic" })));
    updateTimeline(qc, ISSUE, (rows) => [...(rows ?? []), entry("ccc")]);
    restoreTimeline(qc, ISSUE, prev);

    expect(readTimeline(qc, ISSUE)).toEqual([a]);
    expect(table(qc)?.byId).not.toHaveProperty("ccc");
  });

  it("restoring an absent snapshot is a no-op", () => {
    ingestTimeline(qc, ISSUE, [entry("a")]);
    restoreTimeline(qc, ISSUE, undefined);
    expect(ids(qc)).toEqual(["a"]);
  });

  it("keeps issues apart", () => {
    ingestTimeline(qc, ISSUE, [entry("a")]);
    ingestTimeline(qc, OTHER, [entry("zz")]);

    updateTimeline(qc, ISSUE, () => []);

    expect(readTimeline(qc, ISSUE)).toEqual([]);
    expect(readTimeline(qc, OTHER)).toHaveLength(1);
  });

  it("an aborted fetch does not write the store", () => {
    const controller = new AbortController();
    controller.abort();
    ingestTimeline(qc, ISSUE, [entry("a")], controller.signal);
    expect(ids(qc)).toBeUndefined();
    expect(table(qc)).toBeUndefined();
  });

  it("reads undefined before any load", () => {
    expect(readTimeline(qc, ISSUE)).toBeUndefined();
  });
});
