import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import { issueKeys } from "@multica/core/issues/queries";
import { useIssueTimeline } from "./use-issue-timeline";

const realtime = vi.hoisted(() => ({
  events: new Map<string, (payload: unknown) => void>(),
  reconnect: null as (() => void | Promise<unknown>) | null,
}));

vi.mock("@multica/core/realtime", () => ({
  useWSEvent: (event: string, handler: (payload: unknown) => void) => {
    realtime.events.set(event, handler);
  },
  useWSReconnect: (handler: () => void | Promise<unknown>) => {
    realtime.reconnect = handler;
  },
}));

vi.mock("@multica/core/issues/mutations", () => ({
  useCreateComment: () => ({ mutateAsync: vi.fn(), mutate: vi.fn(), isPending: false }),
  useUpdateComment: () => ({ mutateAsync: vi.fn(), mutate: vi.fn(), isPending: false }),
  useDeleteComment: () => ({ mutateAsync: vi.fn(), mutate: vi.fn(), isPending: false }),
  useResolveComment: () => ({ mutateAsync: vi.fn(), mutate: vi.fn(), isPending: false }),
  useToggleCommentReaction: () => ({ mutateAsync: vi.fn(), mutate: vi.fn(), isPending: false }),
}));

function createWrapper(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

describe("useIssueTimeline reconnect race", () => {
  let qc: QueryClient;

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    realtime.events.clear();
    realtime.reconnect = null;
  });

  afterEach(() => {
    qc.clear();
    realtime.events.clear();
    realtime.reconnect = null;
    setApiInstance(undefined as unknown as ApiClient);
  });

  it("keeps a comment event when the older reconnect timeline response resolves", async () => {
    const oldEntry = { type: "activity", id: "activity-old" };
    const newEntry = { type: "comment", id: "comment-new" };
    let resolveReconnect!: (entries: unknown[]) => void;
    const listTimeline = vi.fn()
      .mockResolvedValueOnce([oldEntry])
      .mockImplementationOnce(() => new Promise((resolve) => { resolveReconnect = resolve; }))
      .mockResolvedValue([oldEntry, newEntry]);
    setApiInstance({ listTimeline } as unknown as ApiClient);

    const { unmount } = renderHook(() => useIssueTimeline("issue-race"), {
      wrapper: createWrapper(qc),
    });
    const queryKey = issueKeys.timeline("issue-race");
    await waitFor(() => expect(qc.getQueryState(queryKey)?.fetchStatus).toBe("idle"));
    await act(async () => { await realtime.reconnect?.(); });
    await waitFor(() => expect(listTimeline).toHaveBeenCalledTimes(2));

    act(() => {
      realtime.events.get("comment:created")?.({
        comment: {
          id: "comment-new",
          issue_id: "issue-race",
          author_type: "member",
          author_id: "user-1",
          content: "new comment",
          parent_id: null,
          created_at: "2026-11-04T00:01:00Z",
          updated_at: "2026-11-04T00:01:00Z",
          type: "comment",
          reactions: [],
          attachments: [],
        },
      });
    });
    await act(async () => {
      resolveReconnect([oldEntry]);
      await new Promise((resolve) => setTimeout(resolve, 0));
    });

    expect(listTimeline).toHaveBeenCalledTimes(2);
    expect((qc.getQueryData<{ id: string }[]>(queryKey) ?? []).map((entry) => entry.id))
      .toEqual([oldEntry.id, newEntry.id]);
    unmount();
  });
});
