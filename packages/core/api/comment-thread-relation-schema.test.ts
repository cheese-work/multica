// @vitest-environment node

import { describe, expect, it } from "vitest";
import { CommentSchema, CommentsListSchema } from "./schemas";

const root = {
  id: "root",
  issue_id: "issue",
  author_type: "agent",
  author_id: "agent",
  content: "quote",
  type: "comment",
  parent_id: null,
  created_at: "now",
  updated_at: "now",
};

describe("comment thread_relation (CHE-6428)", () => {
  it("keeps a well-formed relation on a root", () => {
    const relation = { with_me: true, agents_only: false, waiting_on_me: true };
    expect(CommentSchema.parse({ ...root, thread_relation: relation }).thread_relation).toEqual(relation);
  });

  it("leaves the field absent for older servers and for a failed relation read", () => {
    expect(CommentSchema.parse(root).thread_relation).toBeUndefined();
  });

  it("drops a malformed relation instead of failing the comment", () => {
    const bad: unknown[] = [
      null,
      "with_me",
      7,
      [],
      {},
      { with_me: true },
      { with_me: "yes", agents_only: false, waiting_on_me: false },
      { with_me: true, agents_only: null, waiting_on_me: false },
    ];
    for (const thread_relation of bad) {
      const parsed = CommentSchema.parse({ ...root, thread_relation });
      expect(parsed.thread_relation).toBeUndefined();
      expect(parsed.id).toBe("root");
    }
  });

  it("keeps unknown relation fields from a newer server", () => {
    const parsed = CommentsListSchema.parse([
      { ...root, thread_relation: { with_me: false, agents_only: true, waiting_on_me: false, snoozed: true } },
    ]);
    expect(parsed[0]?.thread_relation).toMatchObject({ with_me: false, agents_only: true, waiting_on_me: false });
  });
});
