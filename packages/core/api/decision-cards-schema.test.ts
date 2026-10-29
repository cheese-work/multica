// @vitest-environment node

import { describe, expect, it } from "vitest";
import { ChatMessageSchema, ChatMessagesPageSchema, CommentSchema, TimelineEntriesSchema } from "./schemas";

const card = {
  answer_id: "answer",
  issue_id: "issue",
  issue_identifier: "CHE-1",
  issue_comment_id: "comment",
  class: "reversible",
  state: "pending",
  effective_at: "2026-10-20T05:00:00Z",
};

const bubble = {
  id: "m1",
  chat_session_id: "s1",
  role: "user",
  content: "Yes",
  task_id: null,
  created_at: "2026-10-20T05:00:00Z",
};

describe("DM decision cards", () => {
  it("recognizes decision requests and cancellation notices without downgrading their kind", () => {
    for (const kind of ["decision_request", "decision_cancel_notice"]) {
      expect(ChatMessageSchema.parse({ ...bubble, message_kind: kind }).message_kind).toBe(kind);
    }
  });

  it("drops malformed request metadata without hiding the original ask", () => {
    const parsed = ChatMessageSchema.parse({ ...bubble, decision_request: { id: 7 } });
    expect(parsed.content).toBe("Yes");
    expect(parsed.decision_request).toBeUndefined();
    expect(ChatMessageSchema.parse({ ...bubble, decision_requests: [{ id: 7 }] }).decision_requests).toBeUndefined();
  });

  it("keeps the record comment of an answered request and drops a malformed one", () => {
    const request = { id: "request", issue_id: "issue", ask: "Ask", created_at: bubble.created_at, state: "answered" };
    expect(ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, record_comment_id: "record" } }).decision_request?.record_comment_id).toBe("record");
    expect(ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, record_comment_id: 7 } }).decision_request?.record_comment_id).toBeUndefined();
  });

  it("keeps the asking agent name and drops a malformed one", () => {
    const request = { id: "request", issue_id: "issue", ask: "Ask", created_at: bubble.created_at, state: "open" };
    expect(ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, asking_agent_name: "Sol" } }).decision_request?.asking_agent_name).toBe("Sol");
    expect(ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, asking_agent_name: 7 } }).decision_request?.asking_agent_name).toBeUndefined();
    expect(ChatMessageSchema.parse({ ...bubble, decision_request: request }).decision_request?.asking_agent_name).toBeUndefined();
  });

  it("keeps request state, source and original ask from additive metadata", () => {
    const request = {
      id: "request", issue_id: "issue", source_comment_id: "source",
      ask: "Original ask", created_at: bubble.created_at, state: "withdrawn",
      withdrawn_at: "2026-10-26T06:00:00Z", reason: "Superseded",
    };
    expect(ChatMessageSchema.parse({ ...bubble, decision_request: request }).decision_request).toEqual({
      ...request, issue_identifier: "", options: [],
    });
    expect(ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, state: "future" } }).decision_request?.state).toBe("open");
    expect(ChatMessageSchema.parse(bubble).decision_request).toBeUndefined();
    expect(ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, options: [{ label: "B", what_happens: "ship" }] } }).decision_request?.options)
      .toEqual([{ label: "B", what_happens: "ship" }]);
    // A malformed options list degrades to no buttons, not a dropped card.
    expect(ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, options: "B" } }).decision_request?.options).toEqual([]);
    expect(ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, options: [{ nolabel: 1 }] } }).decision_request?.options).toEqual([]);
    expect(ChatMessageSchema.parse({ ...bubble, decision_requests: [request] }).decision_requests?.[0]?.ask).toBe("Original ask");
  });

  it("keeps decision_reference and its card instead of downgrading to a plain message", () => {
    const parsed = ChatMessageSchema.parse({
      ...bubble,
      role: "assistant",
      message_kind: "decision_reference",
      decision_ref: card,
    });
    expect(parsed.message_kind).toBe("decision_reference");
    expect(parsed.decision_ref?.answer_id).toBe("answer");
  });

  it("leaves decision_quotes absent for older servers and keeps quoted cards", () => {
    expect(ChatMessageSchema.parse(bubble).decision_quotes).toBeUndefined();
    const parsed = ChatMessageSchema.parse({ ...bubble, decision_quotes: [card] });
    expect(parsed.decision_quotes).toHaveLength(1);
    expect(parsed.decision_quotes?.[0]?.issue_identifier).toBe("CHE-1");
  });

  it("degrades a malformed card without hiding the message", () => {
    const parsed = ChatMessageSchema.parse({
      ...bubble,
      decision_quotes: [{ answer_id: 7 }],
      decision_ref: "nope",
    });
    expect(parsed.content).toBe("Yes");
    expect(parsed.decision_quotes).toBeUndefined();
    expect(parsed.decision_ref).toBeUndefined();
  });

  it("falls back to pending for an unknown card state", () => {
    const parsed = ChatMessageSchema.parse({ ...bubble, decision_quotes: [{ ...card, state: "future" }] });
    expect(parsed.decision_quotes?.[0]?.state).toBe("pending");
  });

  it("keeps the inherited flag of a forked card and defaults it to false", () => {
    const parsed = ChatMessageSchema.parse({ ...bubble, decision_quotes: [{ ...card, inherited: true }, card] });
    expect(parsed.decision_quotes?.map((c) => c.inherited)).toEqual([true, false]);
  });

  it("reads has_newer on an anchored page", () => {
    expect(ChatMessagesPageSchema.parse({ messages: [], has_newer: true }).has_newer).toBe(true);
    expect(ChatMessagesPageSchema.parse({ messages: [] }).has_newer).toBeUndefined();
  });
});

describe("issue quote card fields", () => {
  const fields = {
    decision_comment_kind: "quote",
    decision_answer_id: "answer",
    decision_human_user_id: "human",
    decision_recorder_agent_id: "agent",
    decision_class: "deploy",
    decision_state: "effective",
    decision_issue_comment_id: "comment",
    effective_at: "2026-10-20T05:10:00Z",
    disclaimed_at: null,
    decision_source_session_id: "s1",
    decision_source_message_id: "m1",
    decision_request_session_id: "s0",
    decision_request_message_id: "m0",
  };

  it("keeps the fields on a timeline entry", () => {
    const [entry] = TimelineEntriesSchema.parse([
      { type: "comment", id: "c", actor_type: "agent", actor_id: "a", created_at: "now", ...fields },
    ]);
    expect(entry).toMatchObject({ decision_state: "effective", decision_source_message_id: "m1", decision_request_session_id: "s0", decision_request_message_id: "m0", decision_class: "deploy" });
  });

  it("keeps the fields on a comment and tolerates an unknown state", () => {
    const base = {
      id: "c", issue_id: "i", author_type: "agent", author_id: "a", content: "q", type: "comment",
      parent_id: null, created_at: "now", updated_at: "now",
    };
    expect(CommentSchema.parse({ ...base, ...fields }).decision_recorder_agent_id).toBe("agent");
    expect(CommentSchema.parse({ ...base, ...fields, decision_state: "future" }).decision_state).toBeUndefined();
  });
});
