import { describe, expect, it, vi } from "vitest";
import { QueryClient, QueryObserver } from "@tanstack/react-query";
import type { InboxItem } from "@multica/core/types";
import {
  createReadRequestTracker,
  requestMentionReads,
} from "@multica/core/inbox/unread-mention-items";

vi.mock("@/data/api", () => ({ api: {} }));

import { inboxKeys, unreadMentionOptions } from "./inbox";

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

function observe(items: InboxItem[], issueId: string) {
  const qc = new QueryClient();
  qc.setQueryData(inboxKeys.list("ws-1"), items);
  return new QueryObserver(qc, unreadMentionOptions("ws-1", issueId)).getCurrentResult().data;
}

describe("unreadMentionOptions", () => {
  it("is true for an issue with an unread mention", () => {
    expect(observe([item({})], "issue-1")).toBe(true);
  });

  it("is false once the mention is read, archived, or on another issue", () => {
    expect(observe([item({ read: true })], "issue-1")).toBe(false);
    expect(observe([item({ archived: true })], "issue-1")).toBe(false);
    expect(observe([item({})], "issue-2")).toBe(false);
  });

  it("ignores non-mention inbox items", () => {
    expect(observe([item({ type: "new_comment" })], "issue-1")).toBe(false);
  });
});

describe("requestMentionReads", () => {
  it("asks again for a mention whose read failed, and stops once it succeeds", () => {
    const tracker = createReadRequestTracker();
    const items = [item({ id: "a" }), item({ id: "b", issue_id: "issue-2" })];
    const settles: Array<(failed: boolean) => void> = [];
    const send = vi.fn((_id: string, settle: (failed: boolean) => void) => {
      settles.push(settle);
    });

    requestMentionReads(items, "issue-1", tracker, send);
    // In flight: a second pass sends nothing.
    requestMentionReads(items, "issue-1", tracker, send);
    expect(send.mock.calls.map(([id]) => id)).toEqual(["a"]);

    settles[0]!(true);
    requestMentionReads(items, "issue-1", tracker, send);
    expect(send).toHaveBeenCalledTimes(2);

    settles[1]!(false);
    requestMentionReads(items, "issue-1", tracker, send);
    expect(send).toHaveBeenCalledTimes(2);
  });
});
