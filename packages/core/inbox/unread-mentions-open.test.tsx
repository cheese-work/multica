/**
 * @vitest-environment jsdom
 */
import { beforeEach, describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import type { InboxItem } from "../types";
import { inboxKeys } from "./queries";
import { useMarkMentionsReadOnOpen } from "./unread-mentions";

vi.mock("../hooks", () => ({ useWorkspaceId: () => "ws-1" }));

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

describe("useMarkMentionsReadOnOpen", () => {
  let qc: QueryClient;
  let markInboxRead: ReturnType<typeof vi.fn>;
  let listInbox: ReturnType<typeof vi.fn>;

  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  );

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    markInboxRead = vi.fn().mockResolvedValue({});
    listInbox = vi.fn().mockResolvedValue([]);
    setApiInstance({
      markInboxRead,
      listInbox,
      getInboxUnreadSummary: vi.fn().mockResolvedValue([]),
    } as unknown as ApiClient);
  });

  it("marks only this issue's unread mentions read, once", async () => {
    qc.setQueryData(inboxKeys.list("ws-1"), [
      item({ id: "a", issue_id: "issue-1" }),
      item({ id: "b", issue_id: "issue-1" }),
      item({ id: "c", issue_id: "issue-2" }),
      item({ id: "d", issue_id: "issue-1", type: "new_comment" }),
    ]);
    const { rerender } = renderHook(() => useMarkMentionsReadOnOpen("issue-1"), { wrapper });

    await waitFor(() => expect(markInboxRead).toHaveBeenCalledTimes(2));
    rerender();
    expect(markInboxRead.mock.calls.map(([id]) => id).sort()).toEqual(["a", "b"]);
  });

  it("does nothing when the issue has no unread mention", async () => {
    qc.setQueryData(inboxKeys.list("ws-1"), [item({ id: "a", read: true })]);
    renderHook(() => useMarkMentionsReadOnOpen("issue-1"), { wrapper });
    await new Promise((r) => setTimeout(r, 20));
    expect(markInboxRead).not.toHaveBeenCalled();
  });

  it("retries a mention whose read failed, then stops after repeated failures", async () => {
    markInboxRead.mockRejectedValue(new Error("offline"));
    // The server still reports the mention unread after the failed write.
    listInbox.mockResolvedValue([item({ id: "a" })]);
    qc.setQueryData(inboxKeys.list("ws-1"), [item({ id: "a" })]);
    renderHook(() => useMarkMentionsReadOnOpen("issue-1"), { wrapper });

    await waitFor(() => expect(markInboxRead).toHaveBeenCalledTimes(3));
    await new Promise((r) => setTimeout(r, 100));
    expect(markInboxRead).toHaveBeenCalledTimes(3);
  });

  it("recovers when a retry succeeds", async () => {
    markInboxRead.mockRejectedValueOnce(new Error("offline")).mockResolvedValue({});
    listInbox.mockResolvedValue([item({ id: "a" })]);
    qc.setQueryData(inboxKeys.list("ws-1"), [item({ id: "a" })]);
    renderHook(() => useMarkMentionsReadOnOpen("issue-1"), { wrapper });

    await waitFor(() => expect(markInboxRead).toHaveBeenCalledTimes(2));
    await new Promise((r) => setTimeout(r, 50));
    expect(markInboxRead).toHaveBeenCalledTimes(2);
  });
});
