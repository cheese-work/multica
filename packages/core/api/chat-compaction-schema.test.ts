import { describe, expect, it } from "vitest";
import { ChatMessageSchema } from "./schemas";

// L4a (U2): an in-place compaction stores a `compaction_summary` message.
// Clients keep that kind so the list can draw a divider; an unknown kind still
// degrades to a plain message.
describe("compaction_summary message kind", () => {
  const message = {
    id: "m1",
    chat_session_id: "s1",
    role: "assistant",
    content: "Goal: ship.",
    task_id: "t1",
    created_at: "2026-10-28T02:18:00Z",
    message_kind: "compaction_summary",
  };

  it("is kept, not collapsed to a plain message", () => {
    expect(ChatMessageSchema.parse(message).message_kind).toBe("compaction_summary");
  });

  it("falls back to a message for a malformed or unknown kind", () => {
    expect(ChatMessageSchema.parse({ ...message, message_kind: "future-kind" }).message_kind).toBe("message");
    expect(ChatMessageSchema.parse({ ...message, message_kind: 7 }).message_kind).toBe("message");
  });
});
