import { describe, expect, it } from "vitest";
import type { ChatMessage } from "@multica/core/types";
import { openAsksBefore, questionLines } from "./open-asks";

describe("questionLines", () => {
  it("scans a huge line of trailing underscores in linear time", () => {
    const started = performance.now();
    const asks = questionLines(`Which database should we use?${"_".repeat(60_000)}x\nWhich one?${"_".repeat(60_000)}`);
    expect(performance.now() - started).toBeLessThan(200);
    expect(asks).toEqual([]); // lines over 1000 characters are prose, not asks
  });

  it("strips bullets and emphasis markers, skips fences", () => {
    expect(questionLines("- **Which database?**\n```\nwhat?\n```\n2) Why?")).toEqual(["**Which database?", "Why?"]);
  });
});

describe("openAsksBefore", () => {
  const msg = (id: string, role: ChatMessage["role"], content: string, extra: Partial<ChatMessage> = {}): ChatMessage =>
    ({ id, chat_session_id: "s", role, content, task_id: null, created_at: "2026-11-07T00:00:00Z", ...extra });

  it("bounds the work for an assistant message with thousands of questions", () => {
    const content = Array.from({ length: 20_000 }, (_, i) => `Question number ${i}?`).join("\n");
    const messages = [msg("a", "assistant", content), msg("s", "assistant", "SUMMARY", { message_kind: "compaction_summary" })];
    const started = performance.now();
    const asks = openAsksBefore(messages, 1);
    expect(performance.now() - started).toBeLessThan(300);
    expect(asks).toHaveLength(5);
  });

  it("answers a question only when a later member message quotes it", () => {
    const messages = [
      msg("a", "assistant", "Which database?\nWhich cache?"),
      msg("u", "user", "Re: which database?\nPostgres"),
      msg("s", "assistant", "SUMMARY", { message_kind: "compaction_summary" }),
    ];
    expect(openAsksBefore(messages, 2).map((a) => a.ask)).toEqual(["Which cache?"]);
  });
});
