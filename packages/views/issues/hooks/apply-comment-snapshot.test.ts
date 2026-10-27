// @vitest-environment node

import { QueryClient } from "@tanstack/react-query";
import { describe, expect, it } from "vitest";
import { issueKeys } from "@multica/core/issues/queries";
import type { Comment, TimelineEntry } from "@multica/core/types";
import { applyCommentSnapshot } from "./use-issue-timeline";

const quote: TimelineEntry = {
  type: "comment",
  id: "quote",
  actor_type: "agent",
  actor_id: "agent",
  created_at: "2026-10-20T05:00:00Z",
  content: "> Yes",
  revision: 1,
  decision_comment_kind: "quote",
  decision_answer_id: "answer",
  decision_state: "pending",
  decision_class: "reversible",
  decision_source_session_id: "session",
  decision_source_message_id: "message",
};

// A resolve/unresolve snapshot is broadcast, so it never carries the owner-only DM pointers.
const snapshot: Comment = {
  id: "quote",
  issue_id: "issue",
  author_type: "agent",
  author_id: "agent",
  content: "> Yes",
  type: "comment",
  parent_id: null,
  reactions: [],
  attachments: [],
  created_at: "2026-10-20T05:00:00Z",
  updated_at: "2026-10-20T05:01:00Z",
  revision: 2,
  resolved_at: "2026-10-20T05:01:00Z",
  resolved_by_type: "member",
  resolved_by_id: "member",
};

function applied(next: Comment) {
  const qc = new QueryClient();
  qc.setQueryData<TimelineEntry[]>(issueKeys.timeline("issue"), [quote]);
  applyCommentSnapshot(qc, "issue", next);
  return qc.getQueryData<TimelineEntry[]>(issueKeys.timeline("issue"))![0]!;
}

describe("applyCommentSnapshot with decision cards", () => {
  it("keeps the card fields and the owner-only DM pointers a sparse snapshot omits", () => {
    const entry = applied(snapshot);
    expect(entry.resolved_at).toBe("2026-10-20T05:01:00Z");
    expect(entry).toMatchObject({
      decision_state: "pending",
      decision_class: "reversible",
      decision_source_session_id: "session",
      decision_source_message_id: "message",
    });
  });

  it("takes a newer state from the snapshot and still keeps the pointers", () => {
    const entry = applied({ ...snapshot, decision_state: "effective" });
    expect(entry.decision_state).toBe("effective");
    expect(entry.decision_source_message_id).toBe("message");
  });
});
