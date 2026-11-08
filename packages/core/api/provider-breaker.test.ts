// @vitest-environment node
import { afterEach, expect, it, vi } from "vitest";
import { ApiClient } from "./client";
afterEach(() => vi.unstubAllGlobals());
const client = new ApiClient("https://api.example.test");

it("parses breakers and treats an unknown state as open", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          breakers: [
            { provider: "openai", state: "open", failure_count: 3, opened_at: "2026-11-07T02:00:00Z", next_probe_at: null, last_success_at: null, trip_evidence: null },
            { provider: "x", state: "melted" },
          ],
        }),
      ),
    ),
  );
  const [first, second] = await client.listProviderBreakers("ws");
  expect(first).toMatchObject({ provider: "openai", state: "open", failure_count: 3, trip_evidence: [], open_reason: "" });
  expect(second!.state).toBe("open");
});

it("does not present a malformed body as no breakers", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response('{"breakers":"nope"}')));
  await expect(client.listProviderBreakers("ws")).rejects.toThrow("Could not load provider breakers");
});

it("posts the reset to the provider path", async () => {
  const fetchMock = vi.fn().mockResolvedValue(new Response("{}"));
  vi.stubGlobal("fetch", fetchMock);
  await client.resetProviderBreaker("ws", "open ai");
  const [url, init] = fetchMock.mock.calls[0]!;
  expect(String(url)).toBe("https://api.example.test/api/workspaces/ws/provider-breakers/open%20ai/reset");
  expect(init.method).toBe("POST");
});
