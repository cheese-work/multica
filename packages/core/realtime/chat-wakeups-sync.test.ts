import { QueryClient } from "@tanstack/react-query";
import { describe, expect, it, vi } from "vitest";
import { chatKeys } from "../chat/queries";
import { applyChatDoneToCache, applyChatMessageToCache } from "./use-realtime-sync";

const sessionId = "session-1";

function invalidatedKeys(qc: QueryClient) {
  const spy = vi.spyOn(qc, "invalidateQueries");
  return () => spy.mock.calls.map((call) => call[0]?.queryKey);
}

describe("DM follow-up list refresh", () => {
  it("refetches the follow-ups when a turn finishes (the turn may have scheduled one)", () => {
    const qc = new QueryClient();
    const keys = invalidatedKeys(qc);
    applyChatDoneToCache(qc, {
      chat_session_id: sessionId,
      task_id: "t1",
      message_id: "m1",
      content: "ok",
      created_at: "2026-10-21T04:00:00Z",
    });
    expect(keys()).toContainEqual(chatKeys.wakeups(sessionId));
  });

  it("refetches the follow-ups when a turn starts (a follow-up that fired is no longer pending)", () => {
    const qc = new QueryClient();
    const keys = invalidatedKeys(qc);
    applyChatMessageToCache(qc, { chat_session_id: sessionId, role: "user", message_id: "m2", content: "hi", created_at: "2026-10-21T04:00:00Z" });
    expect(keys()).toContainEqual(chatKeys.wakeups(sessionId));
  });
});
