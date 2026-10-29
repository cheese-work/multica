/** @vitest-environment jsdom */
import { QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import type { WSClient } from "../api/ws-client";
import { chatKeys, chatSessionsOptions } from "../chat/queries";
import { createQueryClient } from "../query-client";
import type { ChatSession } from "../types/chat";
import type { ChatMessageEventPayload } from "../types/events";
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

const readSession: ChatSession = {
  id: "dm-1",
  workspace_id: "ws-1",
  agent_id: "agent-1",
  creator_id: "u1",
  title: "Briefs",
  status: "active",
  has_unread: false,
  unread_count: 0,
  created_at: "2026-10-14T00:00:00Z",
  updated_at: "2026-10-14T00:00:00Z",
};

describe("useRealtimeSync — chat message session list refresh", () => {
  let queryClient: ReturnType<typeof createQueryClient>;

  beforeEach(() => {
    queryClient = createQueryClient();
  });

  afterEach(() => {
    cleanup();
    queryClient.clear();
    vi.restoreAllMocks();
  });

  async function mountSessionList() {
    const listChatSessions = vi.fn(async (): Promise<ChatSession[]> => [readSession]);
    setApiInstance({ listChatSessions } as unknown as ApiClient);
    const handlers: Record<string, (payload: unknown) => void> = {};
    const ws = {
      on: (event: string, handler: (payload: unknown) => void) => {
        handlers[event] = handler;
        return () => {
          delete handlers[event];
        };
      },
      onAny: () => () => {},
      onReconnect: () => () => {},
    } as unknown as WSClient;
    const stores = {
      authStore: { getState: () => ({ user: { id: "u1" } }) },
    } as unknown as RealtimeSyncStores;
    function Wrapper({ children }: { children: ReactNode }) {
      return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
    }
    const sessionList = renderHook(
      () => {
        useRealtimeSync(ws, stores);
        return useQuery(chatSessionsOptions("ws-1"));
      },
      { wrapper: Wrapper },
    );
    await waitFor(() => expect(sessionList.result.current.data).toEqual([readSession]));
    expect(listChatSessions).toHaveBeenCalledTimes(1);
    const emitMessage = async (sessionId: string, role: ChatMessageEventPayload["role"], kind?: ChatMessageEventPayload["message_kind"]) => {
      const payload: ChatMessageEventPayload = {
        chat_session_id: sessionId,
        message_id: "message-1",
        role,
        content: "A decision is needed",
        created_at: "2026-10-14T00:01:00Z",
        ...(kind ? { message_kind: kind } : {}),
      };
      await act(async () => {
        handlers["chat:message"]!(payload);
      });
    };
    return { sessionList, listChatSessions, emitMessage, handlers };
  }

  it("refetches a cached DM and shows unread when an assistant ping arrives", async () => {
    const { sessionList, listChatSessions, emitMessage } = await mountSessionList();
    const unreadSession = { ...readSession, has_unread: true, unread_count: 1 };
    listChatSessions.mockResolvedValue([unreadSession]);

    await emitMessage(readSession.id, "assistant");

    await waitFor(() => expect(listChatSessions).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(sessionList.result.current.data).toEqual([unreadSession]));
    expect(listChatSessions).toHaveBeenLastCalledWith({ status: "all" });
  });

  it("refetches the list so a previously uncached DM appears after an assistant ping", async () => {
    const { sessionList, listChatSessions, emitMessage } = await mountSessionList();
    const newSession = { ...readSession, id: "dm-2", has_unread: true, unread_count: 1 };
    listChatSessions.mockResolvedValue([newSession, readSession]);

    await emitMessage(newSession.id, "assistant");

    await waitFor(() => expect(listChatSessions).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(sessionList.result.current.data).toEqual([newSession, readSession]));
  });

  it("does not invalidate or refetch the DM list for a user message", async () => {
    const { sessionList, listChatSessions, emitMessage } = await mountSessionList();
    listChatSessions.mockResolvedValue([{ ...readSession, has_unread: true }]);

    await emitMessage(readSession.id, "user");

    expect(listChatSessions).toHaveBeenCalledTimes(1);
    expect(queryClient.getQueryState(chatKeys.sessions("ws-1"))?.isInvalidated).toBe(false);
    expect(sessionList.result.current.data).toEqual([readSession]);
  });

  it.each(["decision_cancel_notice", "decision_reference"] as const)("does not badge or bump a live %s", async (kind) => {
    const { sessionList, listChatSessions, emitMessage, handlers } = await mountSessionList();
    const openKey = ["chat", "ws-1", "open-decisions"];
    queryClient.setQueryData(openKey, [{ id: "ask" }]);
    await emitMessage(readSession.id, "assistant", kind);
    await act(async () => {
      handlers["chat:done"]!({ chat_session_id: readSession.id, task_id: "task", message_kind: kind });
    });
    expect(listChatSessions).toHaveBeenCalledTimes(1);
    expect(sessionList.result.current.data).toEqual([readSession]);
    expect(queryClient.getQueryState(openKey)?.isInvalidated).toBe(true);
  });

  it("refreshes open asks when expiry posts a decision comment without a DM note", async () => {
    const { handlers, listChatSessions } = await mountSessionList();
    const openKey = ["chat", "ws-1", "open-decisions"];
    queryClient.setQueryData(openKey, [{ id: "expired-ask" }]);
    await act(async () => {
      handlers["comment:created"]!({
        comment: { id: "expiry", issue_id: "issue", decision_request_id: "expired-ask", decision_comment_kind: "cancel" },
      });
    });
    expect(queryClient.getQueryState(openKey)?.isInvalidated).toBe(true);
    expect(listChatSessions).toHaveBeenCalledTimes(1);
  });
});
