// @vitest-environment node

import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());

const sessionId = "11111111-1111-4111-8111-111111111111";
const wakeupId = "22222222-2222-4222-8222-222222222222";

function stubFetch(body: unknown, status = 200) {
  const fetchMock = vi.fn().mockResolvedValue(
    new Response(body === null ? null : JSON.stringify(body), {
      status,
      headers: { "Content-Type": "application/json" },
    }),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

describe("chat session wakeups", () => {
  it("lists pending wakeups from the session endpoint", async () => {
    const row = { id: wakeupId, agent_id: "a-1", fire_at: "2026-10-29T07:00:00Z", instruction: "ping", status: "pending" };
    const fetchMock = stubFetch([row]);
    const result = await new ApiClient("https://api.example.test").listChatSessionWakeups(sessionId);
    expect(fetchMock.mock.calls[0]?.[0]).toBe(`https://api.example.test/api/chat/sessions/${sessionId}/wakeups`);
    expect(result).toEqual([row]);
  });

  it("falls back to an empty list on a malformed response", async () => {
    stubFetch({ unexpected: true });
    expect(await new ApiClient("https://api.example.test").listChatSessionWakeups(sessionId)).toEqual([]);
  });

  it("cancels with DELETE on the wakeup", async () => {
    const fetchMock = stubFetch(null, 204);
    await new ApiClient("https://api.example.test").cancelChatSessionWakeup(sessionId, wakeupId);
    expect(fetchMock.mock.calls[0]?.[0]).toBe(`https://api.example.test/api/chat/sessions/${sessionId}/wakeups/${wakeupId}`);
    expect(fetchMock.mock.calls[0]?.[1]?.method).toBe("DELETE");
  });
});
