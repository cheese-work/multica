import type { ReactNode } from "react";
import { act, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import { chatKeys } from "@multica/core/chat/queries";
import type { ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import { useChatRename } from "./use-chat-rename";

const mocks = vi.hoisted(() => ({ update: vi.fn(), error: vi.fn() }));
vi.mock("@multica/core/api", () => ({ api: { updateChatSession: mocks.update } }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("sonner", () => ({ toast: { error: mocks.error } }));

const session = { id: "session-1", title: "Original title" } as ChatSession;

function setup(current: ChatSession | null = session) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  client.setQueryData(chatKeys.sessions("ws-1"), [session]);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>{children}</I18nProvider>
    </QueryClientProvider>
  );
  return { client, ...renderHook(() => useChatRename(current), { wrapper }) };
}

describe("useChatRename", () => {
  beforeEach(() => {
    mocks.update.mockReset();
    mocks.error.mockReset();
  });

  it("updates the existing title and shared session cache", async () => {
    mocks.update.mockResolvedValue({ ...session, title: "New title" });
    const { result, client } = setup();
    await act(async () => { expect(await result.current.handleRename("  New title  ")).toBe(true); });
    expect(mocks.update).toHaveBeenCalledWith(session.id, { title: "New title" });
    expect(client.getQueryData<ChatSession[]>(chatKeys.sessions("ws-1"))?.[0]?.title).toBe("New title");
  });

  it("reports failure and rolls back the shared title", async () => {
    mocks.update.mockRejectedValue(new Error("offline"));
    const { result, client } = setup();
    await act(async () => { expect(await result.current.handleRename("New title")).toBe(false); });
    expect(mocks.error).toHaveBeenCalledWith(enChat.input.rename_failed_toast);
    expect(client.getQueryData<ChatSession[]>(chatKeys.sessions("ws-1"))?.[0]?.title).toBe(session.title);
  });

  it("requests inline rename for a bare command without an API call", async () => {
    const { result } = setup();
    await act(async () => { expect(await result.current.handleRename("")).toBe(true); });
    expect(result.current.renameRequest).toBe(1);
    expect(mocks.update).not.toHaveBeenCalled();
  });

  it("does not update an unchanged title", async () => {
    const { result } = setup();
    await act(async () => { expect(await result.current.handleRename(session.title!)).toBe(true); });
    expect(mocks.update).not.toHaveBeenCalled();
  });

  it("accepts the inline rename limit of 200 characters", async () => {
    mocks.update.mockResolvedValue(session);
    const { result } = setup();
    await act(async () => { expect(await result.current.handleRename("a".repeat(200))).toBe(true); });
    expect(mocks.update).toHaveBeenCalledWith(session.id, { title: "a".repeat(200) });
  });

  it("rejects a title longer than the inline rename limit", async () => {
    const { result } = setup();
    await act(async () => { expect(await result.current.handleRename("a".repeat(201))).toBe(false); });
    expect(mocks.update).not.toHaveBeenCalled();
    expect(mocks.error).toHaveBeenCalledWith(enChat.input.rename_title_too_long_toast);
  });

  it("keeps a command local when no chat session exists", async () => {
    const { result } = setup(null);
    await act(async () => { expect(await result.current.handleRename("New title")).toBe(false); });
    expect(mocks.update).not.toHaveBeenCalled();
    expect(mocks.error).toHaveBeenCalledWith(enChat.input.rename_session_required_toast);
  });
});
