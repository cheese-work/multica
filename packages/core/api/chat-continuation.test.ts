// @vitest-environment node

import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";
import { parseWithFallback } from "./schema";
import {
  ChatContinueResponseSchema,
  ChatForkResponseSchema,
  EMPTY_CHAT_FORK_RESPONSE,
  ChatMessageSchema,
  ChatSessionSchema,
  ChatSessionUpdatedPayloadSchema,
  EMPTY_CHAT_CONTINUE_RESPONSE,
} from "./schemas";

afterEach(() => vi.unstubAllGlobals());

describe("chat fork contracts", () => {
  const childId = "11111111-1111-4111-8111-111111111111";
  const sourceId = "22222222-2222-4222-8222-222222222222";

  it("posts the idempotency key and message to the fork endpoint", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({
      session: { id: childId, forked_from_session_id: sourceId, fork_last_message_id: "33333333-3333-4333-8333-333333333333", fork_message_count: 4 },
      previous_session_id: sourceId,
      url: "/acme/chat?session=" + childId,
      message: { id: "m10", chat_session_id: childId, role: "user", content: "hi" },
    }), { status: 201, headers: { "Content-Type": "application/json" } }));
    vi.stubGlobal("fetch", fetchMock);
    const result = await new ApiClient("https://api.example.test").forkChatSession(sourceId, {
      idempotency_key: "key-1",
      message: "hi",
    });
    const [url, init] = fetchMock.mock.calls[0] ?? [];
    expect(String(url)).toBe(`https://api.example.test/api/chat/sessions/${sourceId}/fork`);
    expect(init?.method).toBe("POST");
    expect(JSON.parse(String(init?.body))).toEqual({ idempotency_key: "key-1", message: "hi" });
    expect(result.session.fork_last_message_id).toBe("33333333-3333-4333-8333-333333333333");
    expect(result.session.fork_message_count).toBe(4);
    expect(result.message?.id).toBe("m10");
  });

  it("posts attachment ids and keeps the bound ids from the response", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({
      session: { id: childId }, previous_session_id: sourceId, url: "/x", attachment_ids: ["att-1"],
    }), { status: 201, headers: { "Content-Type": "application/json" } }));
    vi.stubGlobal("fetch", fetchMock);
    const result = await new ApiClient("https://api.example.test").forkChatSession(sourceId, {
      idempotency_key: "k", attachment_ids: ["att-1"],
    });
    expect(JSON.parse(String(fetchMock.mock.calls[0]?.[1]?.body))).toEqual({ idempotency_key: "k", attachment_ids: ["att-1"] });
    expect(result.attachment_ids).toEqual(["att-1"]);
  });

  it("drops a malformed attachment_ids field and defaults it when absent", () => {
    for (const value of ["att-1", [1, 2], {}]) {
      expect(ChatForkResponseSchema.parse({ session: { id: childId }, attachment_ids: value }).attachment_ids).toBeUndefined();
    }
    expect(ChatForkResponseSchema.parse({ session: { id: childId } }).attachment_ids).toBeUndefined();
  });

  it("falls back to the empty response when the server body is malformed", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ session: null, url: 7 }), {
      status: 201, headers: { "Content-Type": "application/json" },
    })));
    const result = await new ApiClient("https://api.example.test").forkChatSession(sourceId, { idempotency_key: "k" });
    expect(result).toEqual(EMPTY_CHAT_FORK_RESPONSE);
  });

  it("keeps a valid response without a first message", () => {
    const parsed = ChatForkResponseSchema.parse({ session: { id: childId }, previous_session_id: sourceId, url: "/x", message: null });
    expect(parsed.message).toBeNull();
    expect(ChatForkResponseSchema.parse({ session: { id: childId } }).message).toBeUndefined();
  });
});

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
  it("retains the source message a fork copied up to and drops malformed values", () => {
    const messageID = "44444444-4444-4444-8444-444444444444";
    expect(ChatSessionSchema.parse({ id: "fork", fork_source_message_id: messageID }).fork_source_message_id).toBe(messageID);
    for (const value of ["../../other", 12, {}]) {
      expect(ChatSessionSchema.parse({ id: "fork", fork_source_message_id: value }).fork_source_message_id).toBeUndefined();
    }
  });
  it("preserves a continuation summary and ignores an unknown future kind", () => {
    const message = { id: "message", chat_session_id: "chat", role: "assistant", message_kind: "continuation_summary" };
    expect(ChatMessageSchema.parse(message).message_kind).toBe("continuation_summary");
    expect(ChatMessageSchema.parse({ ...message, message_kind: "future-kind" }).message_kind).toBe("message");
  });

  it("keeps a valid carried_from_session_id and drops absent, null and malformed values", () => {
    const message = { id: "message", chat_session_id: "chat", role: "assistant" };
    const origin = "11111111-1111-4111-8111-111111111111";
    expect(ChatMessageSchema.parse({ ...message, carried_from_session_id: origin }).carried_from_session_id).toBe(origin);
    expect(ChatMessageSchema.parse(message).carried_from_session_id).toBeUndefined();
    expect(ChatMessageSchema.parse({ ...message, carried_from_session_id: null }).carried_from_session_id).toBeNull();
    for (const value of [{}, 42, [], true]) {
      expect(ChatMessageSchema.parse({ ...message, carried_from_session_id: value }).carried_from_session_id).toBeUndefined();
    }
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
