// @vitest-environment node
import { describe, expect, it } from "vitest";
import { ChatMessageSchema, ChatMessagesPageSchema } from "./schemas";

const bubble = {
  id: "m1",
  chat_session_id: "s1",
  role: "user",
  content: "[Scheduled wakeup — x]\nInstruction: report",
  task_id: null,
  created_at: "2026-10-29T10:00:00Z",
};
const wakeup = {
  id: "w1",
  agent_id: "a1",
  title: "",
  instruction: "report",
  fire_at: "2026-10-29T10:00:00Z",
  created_at: "2026-10-29T03:10:00Z",
};

describe("ChatMessage.scheduled_wakeup", () => {
  it("passes a well-formed wakeup through", () => {
    expect(ChatMessageSchema.parse({ ...bubble, scheduled_wakeup: wakeup }).scheduled_wakeup).toMatchObject(wakeup);
  });

  it("defaults a missing title to empty and accepts null/absent", () => {
    const { title: _title, ...noTitle } = wakeup;
    expect(ChatMessageSchema.parse({ ...bubble, scheduled_wakeup: noTitle }).scheduled_wakeup?.title).toBe("");
    expect(ChatMessageSchema.parse({ ...bubble, scheduled_wakeup: null }).scheduled_wakeup).toBeNull();
    expect(ChatMessageSchema.parse(bubble).scheduled_wakeup).toBeUndefined();
  });

  it("degrades a malformed wakeup to a normal message instead of failing the row", () => {
    for (const bad of [{ id: 7 }, "x", { ...wakeup, instruction: null }, { ...wakeup, fire_at: undefined }, []]) {
      const parsed = ChatMessageSchema.parse({ ...bubble, scheduled_wakeup: bad });
      expect(parsed.scheduled_wakeup).toBeUndefined();
      expect(parsed.id).toBe("m1");
    }
  });

  it("keeps the rest of a page when one message carries a malformed wakeup", () => {
    const page = ChatMessagesPageSchema.parse({
      messages: [{ ...bubble, scheduled_wakeup: { id: 7 } }, { ...bubble, id: "m2", scheduled_wakeup: wakeup }],
    });
    expect(page.messages.map((m) => m.scheduled_wakeup?.id)).toEqual([undefined, "w1"]);
  });
});
