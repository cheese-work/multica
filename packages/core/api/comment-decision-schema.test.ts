// @vitest-environment node

import { describe, expect, it } from "vitest";
import { CommentSchema } from "./schemas";

const comment = {
  id: "comment",
  issue_id: "issue",
  author_type: "agent",
  author_id: "agent",
  content: "quote",
  type: "comment",
  parent_id: null,
  created_at: "now",
  updated_at: "now",
};

describe("comment decision stamps", () => {
  it("accepts older comments without stamps", () => {
    expect(CommentSchema.parse(comment).decision_answer_id).toBeUndefined();
  });

  it("keeps valid stamps and tolerates malformed or unknown optional metadata", () => {
    const parsed = CommentSchema.parse({
      ...comment,
      decision_answer_id: "answer",
      decision_request_id: 123,
      decision_intent_id: null,
      decision_comment_kind: "future_kind",
    });
    expect(parsed.decision_answer_id).toBe("answer");
    expect(parsed.decision_request_id).toBeUndefined();
    expect(parsed.decision_intent_id).toBeNull();
    expect(parsed.decision_comment_kind).toBeUndefined();
  });
});
