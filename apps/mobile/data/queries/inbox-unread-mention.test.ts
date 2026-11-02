import { describe, expect, it, vi } from "vitest";
import { QueryClient, QueryObserver } from "@tanstack/react-query";
import type { InboxItem } from "@multica/core/types";

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
