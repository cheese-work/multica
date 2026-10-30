// @vitest-environment node
import { afterEach, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());
const client = new ApiClient("https://api.example.test");
const graph = { nodes: [], edges: [], omitted_count: 0, depth: 1 };

it("scopes the lazy relations request to workspace and depth and forwards cancellation", async () => {
  const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify(graph)));
  vi.stubGlobal("fetch", fetch);
  const controller = new AbortController();
  await expect(client.getIssueRelations("workspace", "issue", 1, controller.signal)).resolves.toEqual(graph);
  const url = new URL(fetch.mock.calls[0]![0]);
  expect(url.pathname).toBe("/api/issues/issue/relations");
  expect(url.searchParams.get("workspace_id")).toBe("workspace");
  expect(url.searchParams.get("depth")).toBe("1");
  expect(fetch.mock.calls[0]![1].signal).toBe(controller.signal);
});

it.each([
  { ...graph, depth: 3 },
  { ...graph, omitted_count: -1 },
  { ...graph, nodes: Array.from({ length: 41 }, () => ({ id: "00000000-0000-4000-8000-000000000001", identifier: "CHE-1", title: "title", status: "todo" })) },
  { ...graph, edges: [{ source: "invalid", target: "00000000-0000-4000-8000-000000000002", type: "mention" }] },
])("rejects malformed relations rather than presenting an empty graph", async (payload) => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify(payload))));
  await expect(client.getIssueRelations("workspace", "issue")).rejects.toThrow();
});

it("preserves permission errors", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response('{"error":"forbidden"}', { status: 403 })));
  await expect(client.getIssueRelations("workspace", "issue")).rejects.toThrow();
});

it("renders future edge types as generic relations without inventing a known type", async () => {
  const edge = { source: "00000000-0000-4000-8000-000000000001", target: "00000000-0000-4000-8000-000000000002", type: "future_relation" };
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ ...graph, edges: [edge] }))));
  expect((await client.getIssueRelations("workspace", "issue")).edges).toEqual([{ ...edge, type: "unknown" }]);
});
