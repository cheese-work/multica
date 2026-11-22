// @vitest-environment node
import { afterEach, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());
const client = new ApiClient("https://api.example.test");
const A = "00000000-0000-4000-8000-00000000000a";
const B = "00000000-0000-4000-8000-00000000000b";

const timeline = {
  as_of: "2026-11-13T00:00:00Z",
  window: { from: "2026-10-14T00:00:00Z", to: "2026-11-13T00:00:00Z" },
  issues: [{
    issue_id: A, status: "in_progress", history: "complete", start_known: true,
    actual_start: "2026-11-01T00:00:00Z", actual_end: null, presence_from: null, presence_to: null,
    start_date: null, due_date: null,
    segments: [{ row_id: "r1", status: "in_progress", category: "started", from: "2026-11-01T00:00:00Z", to: null, clipped_start: false, clipped_end: false }],
  }],
  wakeup_events: [{ id: "e1", at: "2026-11-02T00:00:00Z", target_issue_id: A, rule_id: "w1", rule_kind: "condition", condition_type: "other_issue", source_issue_id: B, class: "cross", run_id: "t1" }],
  next_cursor: null,
};
const batch = {
  nodes: [{ id: A, identifier: "CHE-1", title: "a", status: "todo", parent_issue_id: null, context: false }],
  edges: [{ source: A, target: B, type: "dependency" }],
  next_cursor: null,
};
const respond = (payload: unknown) => vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify(payload))));

it("posts the timeline request and parses the response", async () => {
  respond(timeline);
  const body = { scope: { kind: "workspace" as const }, filters: {}, page: { cursor: "c", limit: 200 } };
  const out = await client.getIssueActualTimeline("ws", body);
  const [url, init] = (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls[0]!;
  expect(new URL(url).pathname).toBe("/api/issues/timeline");
  expect(new URL(url).searchParams.get("workspace_id")).toBe("ws");
  expect(init.method).toBe("POST");
  expect(JSON.parse(init.body)).toEqual(body);
  expect(out.issues[0]!.segments[0]!.category).toBe("started");
  expect(out.wakeup_events[0]!.class).toBe("cross");
});

it("falls back for unknown history, category and wakeup class instead of rejecting the page", async () => {
  const future = structuredClone(timeline);
  future.issues[0]!.history = "future_history";
  future.issues[0]!.segments[0]!.category = "future_category";
  future.wakeup_events[0]!.class = "future_class";
  respond(future);
  const out = await client.getIssueActualTimeline("ws", { scope: { kind: "workspace" }, filters: {} });
  expect(out.issues[0]!.history).toBe("none");
  expect(out.issues[0]!.segments[0]!.category).toBe("unknown");
  expect(out.wakeup_events[0]!.class).toBe("unknown_source");
});

it.each([
  ["issues is not an array", { ...timeline, issues: "x" }],
  ["issue id is not a uuid", { ...timeline, issues: [{ ...timeline.issues[0], issue_id: "nope" }] }],
  ["segments missing", { ...timeline, issues: [{ ...timeline.issues[0], segments: undefined }] }],
  ["as_of missing", { ...timeline, as_of: undefined }],
])("rejects a malformed timeline rather than presenting an empty one: %s", async (_name, payload) => {
  respond(payload);
  await expect(client.getIssueActualTimeline("ws", { scope: { kind: "workspace" }, filters: {} })).rejects.toThrow();
});

it("posts the relations batch request and parses dependency edges and context nodes", async () => {
  respond(batch);
  const out = await client.getIssueRelationsBatch("ws", { issue_ids: [A], types: ["dependency"], page: { limit: 1000 } });
  const [url, init] = (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls[0]!;
  expect(new URL(url).pathname).toBe("/api/issues/relations/batch");
  expect(new URL(url).searchParams.get("workspace_id")).toBe("ws");
  expect(JSON.parse(init.body).issue_ids).toEqual([A]);
  expect(out.edges).toEqual([{ source: A, target: B, type: "dependency" }]);
  expect(out.nodes[0]!.context).toBe(false);
});

it("maps a future edge type to unknown and keeps the cursor", async () => {
  respond({ ...batch, edges: [{ source: A, target: B, type: "future_relation" }], next_cursor: "parent|x|y" });
  const out = await client.getIssueRelationsBatch("ws", { issue_ids: [A] });
  expect(out.edges[0]!.type).toBe("unknown");
  expect(out.next_cursor).toBe("parent|x|y");
});

it.each([
  ["nodes is not an array", { ...batch, nodes: null }],
  ["edge endpoint is not a uuid", { ...batch, edges: [{ source: "bad", target: B, type: "parent" }] }],
  ["edges missing", { nodes: [] }],
])("rejects a malformed relations batch rather than presenting an empty graph: %s", async (_name, payload) => {
  respond(payload);
  await expect(client.getIssueRelationsBatch("ws", { issue_ids: [A] })).rejects.toThrow();
});
