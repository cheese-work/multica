// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { InboxItem } from "../types";
import {
  createReadRequestTracker,
  unreadMentionIssueIds,
  unreadMentionItemIds,
} from "./unread-mention-items";

function item(overrides: Partial<InboxItem>): InboxItem {
  return {
    id: "inbox-1",
    workspace_id: "ws-1",
    recipient_type: "member",
    recipient_id: "member-1",
    actor_type: "member",
    actor_id: "member-2",
    type: "mentioned",
    severity: "info",
    issue_id: "issue-1",
    title: "Issue title",
    body: null,
    issue_status: null,
    read: false,
    archived: false,
    created_at: "2026-06-15T08:00:00Z",
    details: null,
    ...overrides,
  };
}

describe("unreadMentionIssueIds", () => {
  it("collects issues with an unread, active mention", () => {
    const ids = unreadMentionIssueIds([
      item({ id: "a", issue_id: "issue-1" }),
      item({ id: "b", issue_id: "issue-2" }),
    ]);
    expect([...ids].sort()).toEqual(["issue-1", "issue-2"]);
  });

  it("ignores read, archived and non-mention items", () => {
    const ids = unreadMentionIssueIds([
      item({ id: "a", issue_id: "read", read: true }),
      item({ id: "b", issue_id: "archived", archived: true }),
      item({ id: "c", issue_id: "comment", type: "new_comment" }),
    ]);
    expect(ids.size).toBe(0);
  });

  it("ignores mentions with no issue", () => {
    expect(unreadMentionIssueIds([item({ issue_id: null })]).size).toBe(0);
  });

  it("keeps an issue flagged while any one of its mentions is unread", () => {
    const ids = unreadMentionIssueIds([
      item({ id: "a", issue_id: "issue-1", read: true }),
      item({ id: "b", issue_id: "issue-1" }),
    ]);
    expect(ids.has("issue-1")).toBe(true);
  });
});

describe("unreadMentionItemIds", () => {
  it("returns only the unread mention items of the given issue", () => {
    const ids = unreadMentionItemIds(
      [
        item({ id: "a", issue_id: "issue-1" }),
        item({ id: "b", issue_id: "issue-1", read: true }),
        item({ id: "c", issue_id: "issue-2" }),
        item({ id: "d", issue_id: "issue-1", type: "new_comment" }),
      ],
      "issue-1",
    );
    expect(ids).toEqual(["a"]);
  });
});

describe("createReadRequestTracker", () => {
  it("claims an id once while its request is in flight", () => {
    const tracker = createReadRequestTracker();
    expect(tracker.claim("a")).toBe(true);
    expect(tracker.claim("a")).toBe(false);
  });

  it("lets a failed id be claimed again, up to a bounded number of failures", () => {
    const tracker = createReadRequestTracker();
    for (let attempt = 0; attempt < 3; attempt++) {
      expect(tracker.claim("a")).toBe(true);
      tracker.settle("a", true);
    }
    expect(tracker.claim("a")).toBe(false);
  });

  it("does not claim an id again once the read succeeded", () => {
    const tracker = createReadRequestTracker();
    expect(tracker.claim("a")).toBe(true);
    tracker.settle("a", false);
    expect(tracker.claim("a")).toBe(false);
  });
});
