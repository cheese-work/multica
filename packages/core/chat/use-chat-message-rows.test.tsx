// @vitest-environment jsdom
import type { ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ChatMessage, ChatMessagesPage } from "../types";

const listChatMessagesPage = vi.fn();
vi.mock("../api", () => ({ api: { listChatMessagesPage, listChatMessages: vi.fn() } }));

const { patchChatMessageInCaches, upsertChatMessageToCaches } = await import("./message-cache");
const { useChatMessageRows } = await import("./use-chat-message-rows");

const sid = "s1";
const msg = (id: string, over: Partial<ChatMessage> = {}): ChatMessage => ({
  id,
  chat_session_id: sid,
  role: "user",
  content: id,
  task_id: "t",
  created_at: "2026-05-13T05:00:00Z",
  ...over,
});
const page = (messages: ChatMessage[], over: Partial<ChatMessagesPage> = {}): ChatMessagesPage => ({
  messages,
  limit: 50,
  has_more: false,
  next_cursor: null,
  ...over,
});

function setup() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  );
  return { qc, ...renderHook(() => useChatMessageRows(sid), { wrapper }) };
}

beforeEach(() => listChatMessagesPage.mockReset());
afterEach(cleanup);

describe("useChatMessageRows", () => {
  it("reads the transcript oldest first from the by-id store", async () => {
    const cursor = { created_at: "2026-05-13T04:00:00Z", id: "m-3" };
    listChatMessagesPage.mockResolvedValue(
      page([msg("m-3"), msg("m-4")], { has_more: true, next_cursor: cursor }),
    );
    const { result } = setup();
    await waitFor(() => expect(result.current.messages.map((m) => m.id)).toEqual(["m-3", "m-4"]));

    listChatMessagesPage.mockResolvedValue(page([msg("m-1"), msg("m-2")]));
    await act(async () => void (await result.current.query.fetchNextPage()));
    await waitFor(() =>
      expect(result.current.messages.map((m) => m.id)).toEqual(["m-1", "m-2", "m-3", "m-4"]),
    );
  });

  it("shows an upsert and a patch without a page rewrite", async () => {
    listChatMessagesPage.mockResolvedValue(page([msg("m-1")]));
    const { result, qc } = setup();
    await waitFor(() => expect(result.current.messages).toHaveLength(1));
    const fetched = qc.getQueryData(["chat", "messages-page", sid]);

    act(() => upsertChatMessageToCaches(qc, sid, msg("m-2")));
    await waitFor(() => expect(result.current.messages.map((m) => m.id)).toEqual(["m-1", "m-2"]));
    act(() => patchChatMessageInCaches(qc, sid, "m-1", (m) => ({ ...m, content: "edited" })));
    await waitFor(() => expect(result.current.messages[0]!.content).toBe("edited"));
    expect(qc.getQueryData(["chat", "messages-page", sid])).toBe(fetched);
  });

  it("keeps the same array while nothing it reads changed", async () => {
    listChatMessagesPage.mockResolvedValue(page([msg("m-1")]));
    const { result, rerender } = setup();
    await waitFor(() => expect(result.current.messages).toHaveLength(1));
    const first = result.current.messages;
    rerender();
    expect(result.current.messages).toBe(first);
  });
});
