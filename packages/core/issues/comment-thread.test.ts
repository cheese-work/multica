// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { Comment } from "../types";
import { commentToTimelineEntry, mergeCommentThread } from "./comment-thread";

const comment: Comment = {
  id: "old", issue_id: "issue", author_type: "member", author_id: "member",
  content: "Historical chart", parent_id: null, type: "comment", reactions: [], attachments: [],
  created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", revision: 1,
  resolved_at: null, resolved_by_type: null, resolved_by_id: null,
};

describe("requested comment thread", () => {
  it("retains a historical target across bounded refetches without duplicating loaded comments", () => {
    const recent = commentToTimelineEntry({ ...comment, id: "recent", created_at: "2026-02-01T00:00:00Z" });
    expect(mergeCommentThread([recent], [comment]).map((entry) => entry.id)).toEqual(["old", "recent"]);
    expect(mergeCommentThread([recent, commentToTimelineEntry(comment)], [comment])).toHaveLength(2);
  });

  it("keeps the newest revision and deletion state when live events and thread reads overlap", () => {
    const live = commentToTimelineEntry({ ...comment, revision: 3, content: "", deleted_at: "2026-02-01T00:00:00Z" });
    expect(mergeCommentThread([live], [comment])[0]).toEqual(live);
    expect(mergeCommentThread([commentToTimelineEntry(comment)], [{ ...comment, revision: 4, content: "Updated chart" }])[0]?.content).toBe("Updated chart");
  });

  it("keeps the fork's decision and supersede fields on every thread entry", () => {
    const decided: Comment = {
      ...comment,
      superseded_by_id: "newer",
      supersedes: [{ id: "older" } as never],
      decision_request_id: "req-1",
      decision_state: "effective",
    };
    const entry = mergeCommentThread([], [decided])[0];
    expect(entry?.superseded_by_id).toBe("newer");
    expect(entry?.supersedes).toEqual([{ id: "older" }]);
    expect(entry?.decision_request_id).toBe("req-1");
    expect(entry?.decision_state).toBe("effective");
  });
});
