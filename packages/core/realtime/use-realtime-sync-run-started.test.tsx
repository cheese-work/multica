/** @vitest-environment jsdom */
import { InfiniteQueryObserver, QueryClientProvider, QueryObserver } from "@tanstack/react-query";
import { act, cleanup, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { WSClient } from "../api/ws-client";
import { chatKeys } from "../chat/queries";
import { createQueryClient } from "../query-client";
import type { ChatMessage } from "../types";
import type { DecisionCard } from "../types/decision";
import { useRealtimeSync, type RealtimeSyncStores } from "./use-realtime-sync";

vi.mock("../platform/workspace-storage", () => ({
  getCurrentWsId: () => "ws-1",
  getCurrentSlug: () => "test-ws",
  createWorkspaceAwareStorage: (adapter: unknown) => adapter,
  registerForWorkspaceRehydration: () => {},
}));
vi.mock("../paths", () => ({
  useHasOnboarded: () => true,
  resolvePostAuthDestination: () => "/",
}));

const card = (issue_id: string) => ({ answer_id: `a-${issue_id}`, issue_id }) as DecisionCard;
const message = (sessionId: string, issueId?: string): ChatMessage => ({
  id: `m-${sessionId}`,
  chat_session_id: sessionId,
  role: "user",
  content: "hi",
  task_id: null,
  created_at: "2026-11-04T00:00:00Z",
  decision_quotes: issueId ? [card(issueId)] : undefined,
});

// CHE-3233: an issue run start refreshed every DM; only DMs holding a decision
// card for that issue can show the run, so only those may refetch.
describe("useRealtimeSync — task:running on an issue", () => {
  const fetches: Record<string, number> = {};
  const queryClient = createQueryClient();
  const handlers: Record<string, (payload: unknown) => void> = {};

  const mount = (sid: string) => {
    const unsub = new QueryObserver(queryClient, {
      queryKey: chatKeys.messages(sid),
      queryFn: async () => {
        fetches[sid] = (fetches[sid] ?? 0) + 1;
        return [message(sid, sid === "dm-1" ? "X" : undefined)];
      },
      staleTime: Infinity,
    }).subscribe(() => {});
    return unsub;
  };
  const run = (issue_id: string) =>
    handlers["task:running"]!({ task_id: "t", agent_id: "a", issue_id, status: "running" });

  beforeEach(() => {
    vi.useFakeTimers();
    queryClient.clear();
    for (const k of Object.keys(fetches)) delete fetches[k];
    queryClient.setQueryData(chatKeys.messages("dm-1"), [message("dm-1", "X")]);
    queryClient.setQueryData(chatKeys.messages("dm-2"), [message("dm-2", "Z")]);
    queryClient.setQueryData(chatKeys.messages("dm-3"), [message("dm-3")]);
    const ws = {
      on: (event: string, handler: (payload: unknown) => void) => {
        handlers[event] = handler;
        return () => {};
      },
      onAny: () => () => {},
      onReconnect: () => () => {},
    } as unknown as WSClient;
    renderHook(
      () => useRealtimeSync(ws, { authStore: { getState: () => ({ user: { id: "u1" } }) } } as unknown as RealtimeSyncStores),
      { wrapper: ({ children }: { children: ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider> },
    );
  });
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("refetches no DM for ten runs of unrelated issues", async () => {
    const unsubs = ["dm-1", "dm-2", "dm-3"].map(mount);
    await vi.advanceTimersByTimeAsync(0);
    for (const k of Object.keys(fetches)) delete fetches[k];
    act(() => { for (let i = 0; i < 10; i++) run(`Y${i}`); });
    await vi.advanceTimersByTimeAsync(300);
    expect(fetches).toEqual({});
    unsubs.forEach((u) => u());
  });

  it("refetches the matching DM once for ten runs of its issue, and no other", async () => {
    const unsubs = ["dm-1", "dm-2", "dm-3"].map(mount);
    await vi.advanceTimersByTimeAsync(0);
    for (const k of Object.keys(fetches)) delete fetches[k];
    act(() => { for (let i = 0; i < 10; i++) run("X"); });
    await vi.advanceTimersByTimeAsync(300);
    expect(fetches).toEqual({ "dm-1": 1 });
    unsubs.forEach((u) => u());
  });

  it("an unmounted matching DM refetches once, when mounted", async () => {
    act(() => { for (let i = 0; i < 10; i++) run("X"); });
    await vi.advanceTimersByTimeAsync(300);
    expect(fetches).toEqual({});
    const unsub = mount("dm-1");
    await vi.advanceTimersByTimeAsync(0);
    expect(fetches).toEqual({ "dm-1": 1 });
    unsub();
  });

  it("refetches a DM with three loaded pages once per page, not once per event", async () => {
    const pageFetches = { n: 0 };
    queryClient.setQueryData(chatKeys.messagesPage("dm-1"), {
      pages: [0, 1, 2].map((i) => ({ messages: [message("dm-1", i === 2 ? "X" : undefined)], limit: 1, has_more: i < 2, next_cursor: i < 2 ? { created_at: `c${i}`, id: `${i}` } : null })),
      pageParams: [null, 1, 2],
    });
    const unsub = new InfiniteQueryObserver(queryClient, {
      queryKey: chatKeys.messagesPage("dm-1"),
      queryFn: async ({ pageParam }: { pageParam: unknown }) => {
        pageFetches.n++;
        return { messages: [message("dm-1", "X")], limit: 1, has_more: pageParam !== 2, next_cursor: pageParam !== 2 ? { created_at: "c", id: "i" } : null };
      },
      initialPageParam: null as unknown,
      getNextPageParam: (last: { has_more: boolean }) => (last.has_more ? 2 : undefined),
      staleTime: Infinity,
    }).subscribe(() => {});
    await vi.advanceTimersByTimeAsync(0);
    pageFetches.n = 0;
    act(() => { for (let i = 0; i < 10; i++) run("X"); });
    await vi.advanceTimersByTimeAsync(300);
    expect(pageFetches.n).toBeLessThanOrEqual(3);
    expect(pageFetches.n).toBeGreaterThan(0);
    unsub();
  });
});
