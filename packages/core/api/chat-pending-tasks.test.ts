// @vitest-environment node

import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";
import { EMPTY_PENDING_CHAT_TASKS } from "./schemas";

afterEach(() => vi.unstubAllGlobals());

const sessionId = "11111111-1111-4111-8111-111111111111";
const taskId = "22222222-2222-4222-8222-222222222222";

function stubFetch(body: unknown) {
  const fetchMock = vi.fn().mockResolvedValue(
    new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

const client = () => new ApiClient("https://api.example.test");

describe("listPendingChatTasks", () => {
  it("reads the endpoint and keeps created_at from a current server", async () => {
    const body = {
      tasks: [
        { task_id: taskId, status: "queued", chat_session_id: sessionId, created_at: "2026-10-21T10:00:00Z" },
      ],
    };
    const fetchMock = stubFetch(body);
    const result = await client().listPendingChatTasks();
    expect(String(fetchMock.mock.calls[0]?.[0])).toBe("https://api.example.test/api/chat/pending-tasks");
    expect(result).toEqual(body);
  });

  it("accepts an older server that omits created_at", async () => {
    stubFetch({ tasks: [{ task_id: taskId, status: "running", chat_session_id: sessionId }] });
    const result = await client().listPendingChatTasks();
    expect(result.tasks).toEqual([{ task_id: taskId, status: "running", chat_session_id: sessionId }]);
    expect(result.tasks[0]?.created_at).toBeUndefined();
  });

  it("keeps status strings and fields the client has never seen", async () => {
    stubFetch({
      tasks: [{ task_id: taskId, status: "deferred", chat_session_id: sessionId, future_field: 1 }],
      future_top: true,
    });
    const result = await client().listPendingChatTasks();
    expect(result.tasks[0]?.status).toBe("deferred");
  });

  it("defaults a missing tasks list to empty", async () => {
    stubFetch({});
    expect(await client().listPendingChatTasks()).toEqual({ tasks: [] });
  });

  it("falls back to the empty list when the body is malformed", async () => {
    stubFetch({ tasks: "none" });
    expect(await client().listPendingChatTasks()).toEqual(EMPTY_PENDING_CHAT_TASKS);
  });

  it("falls back to the empty list when a task row or its timestamp is malformed", async () => {
    stubFetch({ tasks: [{ task_id: taskId, status: "queued", chat_session_id: sessionId, created_at: 5 }] });
    expect(await client().listPendingChatTasks()).toEqual(EMPTY_PENDING_CHAT_TASKS);
    stubFetch({ tasks: [{ status: "queued" }] });
    expect(await client().listPendingChatTasks()).toEqual(EMPTY_PENDING_CHAT_TASKS);
  });
});
