/** @vitest-environment jsdom */
import { QueryClientProvider, QueryObserver, useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import type { WSClient } from "../api/ws-client";
import {
  chatKeys,
  chatMessagesPageOptions,
  chatSessionsOptions,
  pendingChatTaskOptions,
} from "../chat/queries";
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

  async function mountSessionList(listChatSessions = vi.fn(async (): Promise<ChatSession[]> => [readSession])) {
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

  it("heals a mounted DM when the refreshed list has a newer message", async () => {
    const newerSession = {
      ...readSession,
      last_message: {
        id: "00000000-0000-4000-8000-000000000002",
        task_id: "task-1",
        content: "The reply",
        role: "assistant",
        created_at: "2026-10-14T00:02:00Z",
      },
    } as unknown as ChatSession;
    const listChatSessions = vi.fn()
      .mockResolvedValueOnce([readSession])
      .mockResolvedValueOnce([newerSession]);
    const assistantMessage = {
      id: "00000000-0000-4000-8000-000000000002",
      chat_session_id: readSession.id,
      role: "assistant",
      content: "The reply",
      created_at: "2026-10-14T00:02:00Z",
    };
    const userMessage = {
      ...assistantMessage,
      id: "00000000-0000-4000-8000-000000000001",
      role: "user",
      content: "The prompt",
      created_at: "2026-10-14T00:01:00Z",
    };
    const listChatMessagesPage = vi.fn()
      .mockResolvedValueOnce({ messages: [userMessage], limit: 50, has_more: false })
      .mockResolvedValue({ messages: [userMessage, assistantMessage], limit: 50, has_more: false });
    const getPendingChatTask = vi.fn()
      .mockResolvedValueOnce({ task_id: "task-1", status: "dispatched", created_at: "2026-10-14T00:01:00Z" })
      .mockResolvedValue({});
    setApiInstance({ listChatSessions, listChatMessagesPage, getPendingChatTask } as unknown as ApiClient);
    const handlers: Record<string, (payload: unknown) => void> = {};
    const ws = {
      on: (event: string, handler: (payload: unknown) => void) => {
        handlers[event] = handler;
        return () => delete handlers[event];
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
    const mounted = renderHook(() => {
      useRealtimeSync(ws, stores);
      return {
        sessions: useQuery(chatSessionsOptions("ws-1")),
        messages: useInfiniteQuery(chatMessagesPageOptions(readSession.id)),
        pending: useQuery(pendingChatTaskOptions(readSession.id)),
      };
    }, { wrapper: Wrapper });

    await waitFor(() => expect(mounted.result.current.pending.data?.status).toBe("dispatched"));
    await waitFor(() => expect(mounted.result.current.messages.data?.pages[0]?.messages).toHaveLength(1));
    await act(async () => {
      handlers["chat:session_read"]!({ chat_session_id: readSession.id });
    });

    await waitFor(() => expect(listChatSessions).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(listChatMessagesPage).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(getPendingChatTask).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(mounted.result.current.messages.data?.pages[0]?.messages).toContainEqual(assistantMessage));
    expect(mounted.result.current.pending.data).toEqual({});

    await act(async () => {
      handlers["chat:session_read"]!({ chat_session_id: readSession.id });
    });
    await waitFor(() => expect(listChatSessions).toHaveBeenCalledTimes(3));
    expect(listChatMessagesPage).toHaveBeenCalledTimes(2);
    expect(getPendingChatTask).toHaveBeenCalledTimes(2);
  });

  it("marks an unmounted DM detail stale without refetching it", async () => {
    const newerSession = {
      ...readSession,
      last_message: {
        id: "00000000-0000-4000-8000-000000000002",
        task_id: "task-1",
        content: "The reply",
        role: "assistant",
        created_at: "2026-10-14T00:02:00Z",
      },
    } as unknown as ChatSession;
    const listChatSessions = vi.fn()
      .mockResolvedValueOnce([readSession])
      .mockResolvedValueOnce([newerSession]);
    const { handlers } = await mountSessionList(listChatSessions);
    const pageKey = chatKeys.messagesPage(readSession.id);
    const pendingKey = chatKeys.pendingTask(readSession.id);
    queryClient.setQueryData(pageKey, {
      pages: [{ messages: [{ id: "00000000-0000-4000-8000-000000000001" }] }],
      pageParams: [null],
    });
    queryClient.setQueryData(pendingKey, {
      task_id: "task-1",
      status: "dispatched",
      created_at: "2026-10-14T00:01:00Z",
    });

    await act(async () => {
      handlers["chat:session_read"]!({ chat_session_id: readSession.id });
    });
    await waitFor(() => expect(listChatSessions).toHaveBeenCalledTimes(2));

    expect(queryClient.getQueryState(pageKey)?.isInvalidated).toBe(true);
    expect(queryClient.getQueryState(pendingKey)?.isInvalidated).toBe(true);
  });

  it("does not refetch a pending task for an assistant reply from its predecessor", async () => {
    const priorReplySession = {
      ...readSession,
      last_message: {
        id: "00000000-0000-4000-8000-000000000003",
        task_id: "task-1",
        content: "Previous reply",
        role: "assistant",
        created_at: "2026-10-14T00:02:00Z",
      },
    } as unknown as ChatSession;
    const listChatSessions = vi.fn()
      .mockResolvedValueOnce([readSession])
      .mockResolvedValue([priorReplySession]);
    const getPendingChatTask = vi.fn().mockResolvedValue({
      task_id: "task-2",
      status: "running",
      created_at: "2026-10-14T00:01:00Z",
    });
    const { handlers } = await mountSessionList(listChatSessions);
    setApiInstance({ listChatSessions, getPendingChatTask } as unknown as ApiClient);
    const pendingObserver = new QueryObserver(queryClient, pendingChatTaskOptions(readSession.id));
    const unsubscribe = pendingObserver.subscribe(() => undefined);
    try {
      await waitFor(() => expect(getPendingChatTask).toHaveBeenCalledTimes(1));
      await act(async () => {
        handlers["chat:session_read"]!({ chat_session_id: readSession.id });
        await waitFor(() => expect(listChatSessions).toHaveBeenCalledTimes(2));
      });
      expect(getPendingChatTask).toHaveBeenCalledTimes(1);
    } finally {
      unsubscribe();
    }
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
