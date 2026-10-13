// @vitest-environment node

import { describe, expect, it } from "vitest";
import { parseWithFallback } from "./schema";
import {
  ChatContinueResponseSchema,
  ChatMessageSchema,
  ChatSessionSchema,
  ChatSessionUpdatedPayloadSchema,
  EMPTY_CHAT_CONTINUE_RESPONSE,
} from "./schemas";

describe("chat continuation contracts", () => {
  it("retains a fork source and drops malformed fork sources", () => {
    const sourceID = "11111111-1111-4111-8111-111111111111";
    expect(ChatSessionSchema.parse({ id: "fork", forked_from_session_id: sourceID }).forked_from_session_id).toBe(sourceID);
    for (const value of [123, "../../other", {}]) {
      expect(ChatSessionSchema.parse({ id: "fork", forked_from_session_id: value }).forked_from_session_id).toBeUndefined();
    }
    expect(ChatSessionSchema.parse({ id: "source" }).forked_from_session_id).toBeUndefined();
  });
  it("retains the fork point and copy count and drops malformed values", () => {
    const messageID = "22222222-2222-4222-8222-222222222222";
    const fork = ChatSessionSchema.parse({ id: "fork", fork_last_message_id: messageID, fork_message_count: 12 });
    expect(fork.fork_last_message_id).toBe(messageID);
    expect(fork.fork_message_count).toBe(12);
    for (const value of ["../../other", -1, 1.5, "12", {}]) {
      const parsed = ChatSessionSchema.parse({ id: "fork", fork_last_message_id: value, fork_message_count: value });
      expect(parsed.fork_last_message_id).toBeUndefined();
      expect(parsed.fork_message_count).toBeUndefined();
    }
  });
  it("preserves a continuation summary and ignores an unknown future kind", () => {
    const message = { id: "message", chat_session_id: "chat", role: "assistant", message_kind: "continuation_summary" };
    expect(ChatMessageSchema.parse(message).message_kind).toBe("continuation_summary");
    expect(ChatMessageSchema.parse({ ...message, message_kind: "future-kind" }).message_kind).toBe("message");
  });

  it("retains a valid successor and drops a malformed successor", () => {
    const nextSessionId = "11111111-1111-4111-8111-111111111111";
    expect(ChatSessionSchema.parse({ id: "old", continued_into_session_id: nextSessionId }).continued_into_session_id).toBe(nextSessionId);
    expect(ChatSessionSchema.parse({ id: "old", continued_into_session_id: 123 }).continued_into_session_id).toBeUndefined();
    expect(ChatSessionSchema.parse({ id: "old" }).continued_into_session_id).toBeUndefined();
  });

  it("validates predecessor links in session and continuation responses", () => {
    const previousSessionId = "22222222-2222-4222-8222-222222222222";
    const session = { id: "fresh", continued_from_session_id: previousSessionId };
    expect(ChatSessionSchema.parse(session).continued_from_session_id).toBe(previousSessionId);
    expect(ChatContinueResponseSchema.parse({ session, previous_session_id: previousSessionId, url: "/acme/chat?session=fresh" }).session.continued_from_session_id).toBe(previousSessionId);
    expect(ChatSessionSchema.parse({ ...session, continued_from_session_id: "../../other" }).continued_from_session_id).toBeUndefined();
    expect(ChatSessionSchema.parse({ ...session, continued_from_session_id: null }).continued_from_session_id).toBeNull();
    expect(ChatSessionSchema.parse({ id: "legacy" }).continued_from_session_id).toBeUndefined();
  });

  it("validates the continuation response without trusting malformed JSON", () => {
    const response = { session: { id: "fresh" }, previous_session_id: "old", url: "/acme/chat?session=fresh" };
    expect(ChatContinueResponseSchema.parse(response).session.id).toBe("fresh");
    expect(parseWithFallback({ session: null, url: 123 }, ChatContinueResponseSchema, EMPTY_CHAT_CONTINUE_RESPONSE, { endpoint: "POST /api/chat/sessions/:id/continue" })).toEqual(EMPTY_CHAT_CONTINUE_RESPONSE);
  });

  it("drops malformed continuation events without accepting another destination", () => {
    const event = { chat_session_id: "11111111-1111-4111-8111-111111111111", status: "archived", continued_into_session_id: "../../other" };
    expect(ChatSessionUpdatedPayloadSchema.parse(event).continued_into_session_id).toBeUndefined();
    expect(ChatSessionUpdatedPayloadSchema.safeParse({ chat_session_id: 123 }).success).toBe(false);
  });
});
