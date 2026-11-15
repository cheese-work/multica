/**
 * @vitest-environment jsdom
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import { useBatchChatSessions } from "./mutations";
import { chatKeys } from "./queries";
import type { ChatSession } from "../types";

vi.mock("../hooks", () => ({ useWorkspaceId: () => "ws-1" }));

const mk = (id: string, o: Partial<ChatSession> = {}): ChatSession =>
  ({
    id,
    workspace_id: "ws-1",
    agent_id: "a",
    creator_id: "u",
    title: id,
    status: "active",
    pinned: false,
    has_unread: true,
    unread_count: 2,
    created_at: "2026-07-10T00:00:00Z",
    updated_at: "2026-07-10T00:00:00Z",
    ...o,
  }) as ChatSession;

describe("useBatchChatSessions", () => {
  let qc: QueryClient;
  const api = {
    markChatSessionRead: vi.fn(),
    markChatSessionUnread: vi.fn(),
    setChatSessionPinned: vi.fn(),
    setChatSessionArchived: vi.fn(),
  };
  const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  const rows = () => qc.getQueryData<ChatSession[]>(chatKeys.sessions("ws-1"))!;

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    qc.setQueryData(chatKeys.sessions("ws-1"), [mk("a"), mk("b"), mk("c")]);
    for (const f of Object.values(api)) f.mockReset().mockResolvedValue(undefined);
    setApiInstance(api as unknown as ApiClient);
  });
  afterEach(() => qc.clear());

  it("calls the API once per id and patches only those rows", async () => {
    const { result } = renderHook(() => useBatchChatSessions(), { wrapper });
    let out: { failed: string[] } | undefined;
    await act(async () => {
      out = await result.current.mutateAsync({ action: "seen", sessionIds: ["a", "b"] });
    });
    expect(api.markChatSessionRead.mock.calls.map((c) => c[0])).toEqual(["a", "b"]);
    expect(out).toEqual({ failed: [] });
    expect(rows().find((s) => s.id === "a")).toMatchObject({ has_unread: false, unread_count: 0 });
    expect(rows().find((s) => s.id === "c")).toMatchObject({ has_unread: true, unread_count: 2 });
  });

  it("maps pin / unpin / archive / unarchive to the right calls", async () => {
    const { result } = renderHook(() => useBatchChatSessions(), { wrapper });
    await act(async () => {
      await result.current.mutateAsync({ action: "pin", sessionIds: ["a"] });
      await result.current.mutateAsync({ action: "unpin", sessionIds: ["a"] });
      await result.current.mutateAsync({ action: "archive", sessionIds: ["b"] });
      await result.current.mutateAsync({ action: "unarchive", sessionIds: ["b"] });
    });
    expect(api.setChatSessionPinned.mock.calls).toEqual([["a", true], ["a", false]]);
    expect(api.setChatSessionArchived.mock.calls).toEqual([["b", true], ["b", false]]);
  });

  it("returns the failed ids and restores only those rows", async () => {
    api.setChatSessionArchived.mockImplementation((id: string) => (id === "b" ? Promise.reject(new Error("no")) : Promise.resolve()));
    const { result } = renderHook(() => useBatchChatSessions(), { wrapper });
    let out: { failed: string[] } | undefined;
    await act(async () => {
      out = await result.current.mutateAsync({ action: "archive", sessionIds: ["a", "b"] });
    });
    expect(out).toEqual({ failed: ["b"] });
    expect(rows().find((s) => s.id === "b")!.status).toBe("active");
    expect(rows().find((s) => s.id === "a")!.status).toBe("archived");
  });
});
