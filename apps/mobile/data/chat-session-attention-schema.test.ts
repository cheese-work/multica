import { describe, expect, it } from "vitest";
import { ChatSessionSchema } from "./schemas";

const base = { id: "s1" };

describe("ChatSessionSchema attention fields", () => {
  it("keeps the projection the server sent", () => {
    const s = ChatSessionSchema.parse({
      ...base,
      stale_level: "red",
      attention_source: "decision",
      attention_since: "2026-11-13T00:00:00Z",
      attention_keys: ["decision:1@2", "item:3"],
      attention_ask_ids: ["a1"],
    });
    expect(s).toMatchObject({
      stale_level: "red",
      attention_source: "decision",
      attention_keys: ["decision:1@2", "item:3"],
    });
  });

  it("older server: fields stay absent", () => {
    expect(ChatSessionSchema.parse(base).stale_level).toBeUndefined();
  });

  it("unknown enum values downgrade instead of throwing", () => {
    const s = ChatSessionSchema.parse({ ...base, stale_level: "purple", attention_source: "x" });
    expect(s.stale_level).toBe("none");
    expect(s.attention_source).toBeNull();
  });
});
