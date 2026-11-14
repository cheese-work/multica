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

  describe("action preview", () => {
    const request = { id: "request", issue_id: "issue", ask: "Ask", created_at: bubble.created_at, state: "open" };
    const digest = "a".repeat(64);
    const preview = { fields: [{ label: "Run cap", current: "10", proposed: "30" }], sha256: digest };
    const action = { id: "action", action_type: "issue.run_budget.raise", payload_sha256: "payload", preview };
    const parse = (value: unknown) => ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, action: value } }).decision_request?.action;

    it("keeps a well-formed action and its preview", () => {
      expect(parse(action)).toEqual(action);
    });

    it("keeps the action without a preview when the preview is missing or malformed", () => {
      const { preview: _omit, ...bare } = action;
      expect(parse(bare)?.preview).toBeUndefined();
      expect(parse(bare)?.id).toBe("action");
      for (const bad of [
        "x",
        { fields: "x", sha256: digest },
        { fields: [{ label: "Run cap", current: 10, proposed: "30" }], sha256: digest },
        { fields: [{ label: "Run cap", current: "10", proposed: "30" }] },
        { fields: [], sha256: digest },
        { ...preview, sha256: "abc" },
        { ...preview, sha256: "A".repeat(64) },
      ]) {
        expect(parse({ ...action, preview: bad })?.preview).toBeUndefined();
        expect(parse({ ...action, preview: bad })?.payload_sha256).toBe("payload");
      }
    });

    it("drops the preview of an action type this client does not know", () => {
      expect(parse({ ...action, action_type: "future.thing" })?.preview).toBeUndefined();
      expect(parse({ ...action, action_type: undefined })?.preview).toBeUndefined();
      for (const known of ["issue.run_budget.raise", "issue.run_budget.top_up", "issue.project.set", "agent.env.patch", "agent.copy"]) {
        expect(parse({ ...action, action_type: known })?.preview).toEqual(preview);
      }
    });

    it("drops an action that cannot be approved and leaves the card alone", () => {
      expect(parse({ ...action, id: 7 })).toBeUndefined();
      expect(parse({ ...action, payload_sha256: undefined })).toBeUndefined();
      expect(ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, action: "x" } }).decision_request?.id).toBe("request");
    });
  });

  describe("config change", () => {
    const request = { id: "request", issue_id: "issue", ask: "Ask", created_at: bubble.created_at, state: "open" };
    const digest = "a".repeat(64);
    const config_change = {
      field: "workflow.instructions", target_id: "wf-1", keys: [], diff_kind: "text",
      diff: [{ old_start: 1, new_start: 1, lines: ["-old", "+new"] }], after: "new",
      before_sha256: "b".repeat(64), after_sha256: "c".repeat(64), sha256: digest,
    };
    const action = { id: "action", action_type: "workflow.step.instructions.set", payload_sha256: "payload", state: "pending", owner_only: true, config_change };
    const parse = (value: unknown) => ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, action: value } }).decision_request?.action;

    it("keeps the server preview and the owner flag", () => {
      expect(parse(action)).toEqual(action);
    });

    it("drops a malformed change but keeps the action, so the card says details are unavailable", () => {
      for (const bad of [
        "x",
        { ...config_change, sha256: "abc" },
        { ...config_change, diff: "x" },
        { ...config_change, diff: [{ old_start: 1, new_start: 1, lines: [7] }] },
        { ...config_change, field: undefined },
        { ...config_change, keys: "x" },
      ]) {
        const parsed = parse({ ...action, config_change: bad });
        expect(parsed?.config_change).toBeUndefined();
        expect(parsed?.id).toBe("action");
      }
    });

    it("keeps a diff_kind a newer server sends; approval fails closed on it elsewhere", () => {
      const parsed = parse({ ...action, config_change: { ...config_change, diff_kind: "other" } });
      expect(parsed?.config_change?.diff_kind).toBe("other");
      expect(parsed?.id).toBe("action");
    });

    it("treats a missing or malformed owner flag as unknown", () => {
      expect(parse({ ...action, owner_only: undefined })?.owner_only).toBeUndefined();
      expect(parse({ ...action, owner_only: "yes" })?.owner_only).toBeUndefined();
      expect(parse({ ...action, owner_only: false })?.owner_only).toBe(false);
    });
  });

  it("keeps the receipt fields and degrades a malformed answer on its own", () => {
    const request = { id: "request", issue_id: "issue", ask: "Ask", created_at: bubble.created_at, state: "answered" };
    const answer = { state: "delivered", chosen_option_index: 1, option_label: "Hold", answered_at: "2026-10-26T06:00:00Z" };
    expect(ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, answer } }).decision_request?.answer).toEqual(answer);
    expect(ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, answer: "x" } }).decision_request?.answer).toBeUndefined();
    expect(ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, answer: { ...answer, state: "future" } } }).decision_request?.answer?.state).toBe("recorded");
  });

  it("keeps the run that picked the answer up and degrades a malformed run on its own", () => {
    const request = { id: "request", issue_id: "issue", ask: "Ask", created_at: bubble.created_at, state: "answered" };
    const answer = { state: "picked_up", run: { id: "task", agent_name: "Sol" }, answered_at: "2026-10-26T06:00:00Z" };
    const parsed = (next: unknown) => ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, answer: next } }).decision_request?.answer;
    expect(parsed(answer)).toEqual(answer);
    expect(parsed({ ...answer, run: "x" })).toMatchObject({ state: "picked_up", run: undefined });
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
    const detailed = { label: "B", what_happens: "ship", pros: ["safe"], cons: ["slow"], cost_or_time: "2 days" };
    expect(ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, options: [detailed], recommended_option_index: 0 } }).decision_request)
      .toMatchObject({ options: [detailed], recommended_option_index: 0 });
    expect(ChatMessageSchema.parse({ ...bubble, decision_request: { ...request, options: [{ label: "B", pros: "x" }], recommended_option_index: "no" } }).decision_request)
      .toMatchObject({ options: [{ label: "B", pros: [] }], recommended_option_index: undefined });
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
