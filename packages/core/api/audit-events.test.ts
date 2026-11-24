// @vitest-environment node
import { afterEach, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());
const client = new ApiClient("https://api.example.test");
const respond = (body: unknown, status = 200) =>
  vi.fn().mockImplementation(async () => new Response(JSON.stringify(body), { status }));

const row = {
  id: "e1",
  at: "2026-11-09T04:18:00Z",
  actor_type: "agent",
  actor_id: "a1",
  actor_name: "Tofu",
  task_id: "t1",
  kind: "agent.update",
  target_type: "agent",
  target_id: "a2",
  target_name: "Sonnet",
  before: { max_concurrent_tasks: 6 },
  after: { max_concurrent_tasks: 2 },
  external_requested: [],
};

it("sends only the filters that are set, to the workspace's audit path", async () => {
  const fetcher = respond({ events: [], next_cursor: null });
  vi.stubGlobal("fetch", fetcher);
  await client.listAuditEvents("w/1", { kind: "agent.", actor_id: "", cursor: "c1", limit: 50 });
  const url = new URL(fetcher.mock.calls[0]![0]);
  expect(url.pathname).toBe("/api/workspaces/w%2F1/audit-events");
  expect(Object.fromEntries(url.searchParams)).toEqual({ kind: "agent.", cursor: "c1", limit: "50" });
});

it("parses rows and the next cursor", async () => {
  vi.stubGlobal("fetch", respond({ events: [row], next_cursor: "c2" }));
  const page = await client.listAuditEvents("w1");
  expect(page.next_cursor).toBe("c2");
  expect(page.events[0]).toMatchObject({ kind: "agent.update", actor_name: "Tofu", after: { max_concurrent_tasks: 2 } });
});

it("reads null maps, a deleted target and an unknown actor type without failing the page", async () => {
  vi.stubGlobal(
    "fetch",
    respond({ events: [{ ...row, actor_type: "robot", target_name: null, before: null, after: null }], next_cursor: null }),
  );
  const [event] = (await client.listAuditEvents("w1")).events;
  expect(event).toMatchObject({ actor_type: "system", target_name: null, before: {}, after: {} });
});

it("throws when the body is not an audit page", async () => {
  vi.stubGlobal("fetch", respond({ events: "nope" }));
  await expect(client.listAuditEvents("w1")).rejects.toThrow("Could not load the audit log");
});
