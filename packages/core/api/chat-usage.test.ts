// @vitest-environment node

import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";
import { EMPTY_CHAT_SESSION_USAGE } from "./schemas";

afterEach(() => vi.unstubAllGlobals());

const sessionId = "11111111-1111-4111-8111-111111111111";

const usage = {
  session_id: sessionId,
  tokens: { input: 307, output: 133, cache_read: 30, cache_write: 15 },
  uncosted_tokens: { input: 200, output: 80, cache_read: 20, cache_write: 10 },
  cost_usd_ticks: 1500,
  runs: {
    by_status: { completed: 3, failed: 1, cancelled: 0, queued: 1, running: 1 },
    terminal: 4,
    metered: 2,
    total_run_ms: 38000,
  },
  messages: { by_role: { user: 2, assistant: 3 } },
  models: ["m-a", "m-b"],
  continued_from_session_id: null,
  continued_into_session_id: null,
  forked_from_session_id: null,
};

function stubFetch(body: unknown) {
  const fetchMock = vi.fn().mockResolvedValue(
    new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

describe("getChatSessionUsage", () => {
  it("reads the usage endpoint and keeps the server values", async () => {
    const fetchMock = stubFetch(usage);
    const result = await new ApiClient("https://api.example.test").getChatSessionUsage(sessionId);
    expect(String(fetchMock.mock.calls[0]?.[0])).toBe(
      `https://api.example.test/api/chat/sessions/${sessionId}/usage`,
    );
    expect(result).toEqual(usage);
  });

  it("defaults fields a server omits and ignores unknown ones", async () => {
    stubFetch({ session_id: sessionId, tokens: { input: 5 }, future_field: true });
    const result = await new ApiClient("https://api.example.test").getChatSessionUsage(sessionId);
    expect(result.tokens).toEqual({ input: 5, output: 0, cache_read: 0, cache_write: 0 });
    expect(result.runs.by_status).toEqual({});
    expect(result.models).toEqual([]);
    expect(result.continued_from_session_id).toBeNull();
  });

  it("falls back to the empty usage when the body is malformed", async () => {
    stubFetch({ session_id: 7, tokens: "lots", runs: null });
    const result = await new ApiClient("https://api.example.test").getChatSessionUsage(sessionId);
    expect(result).toEqual(EMPTY_CHAT_SESSION_USAGE);
  });
});
