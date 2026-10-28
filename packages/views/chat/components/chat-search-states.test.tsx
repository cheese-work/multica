import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { onlineManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { useChatSearchStore } from "@multica/core/chat/search";
import { useSetChatSessionArchived } from "@multica/core/chat/mutations";
import { chatKeys } from "@multica/core/chat/queries";
import type { Agent, ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import enCommon from "../../locales/en/common.json";
import enIssues from "../../locales/en/issues.json";

const backend = vi.hoisted(() => ({
  archived: true,
  searchChats: vi.fn(),
  setArchived: vi.fn(),
}));

vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => null }));
vi.mock("@multica/core/hooks", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/hooks")>()),
  useWorkspaceId: () => "ws-1",
}));
vi.mock("@multica/core/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      getPendingChatTasks: vi.fn(async () => ({ tasks: [] })),
      searchChats: backend.searchChats,
      setChatSessionArchived: backend.setArchived,
    },
  };
});

import { ChatSearch } from "./chat-search";

const agent = { id: "agent-1", name: "Alpha" } as unknown as Agent;
const archivedSession: ChatSession = {
  id: "one", workspace_id: "ws-1", agent_id: "agent-1", creator_id: "u1",
  title: "Chat one", status: "archived", has_unread: false, unread_count: 0,
  last_message: null, pinned: false,
  created_at: new Date(0).toISOString(), updated_at: new Date(0).toISOString(),
};

function renderSearch(queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  function Harness() {
    const archive = useSetChatSessionArchived();
    return (
      <ChatSearch
        scope="page" sessions={[archivedSession]} agents={[agent]} activeSessionId={null}
        onOpenHit={vi.fn()} onArchive={vi.fn()} onTogglePin={vi.fn()}
        onUnarchive={(session) => archive.mutate({ sessionId: session.id, archived: false })}
      >
        <span>Normal list</span>
      </ChatSearch>
    );
  }
  render(
    <QueryClientProvider client={queryClient}>
      <I18nProvider locale="en" resources={{ en: { chat: enChat, common: enCommon, issues: enIssues } }}>
        <Harness />
      </I18nProvider>
    </QueryClientProvider>,
  );
  return queryClient;
}

beforeEach(() => {
  vi.clearAllMocks();
  useChatSearchStore.setState({ queries: {} });
  backend.archived = true;
  backend.searchChats.mockImplementation(async () => ({
    results: [{
      session_id: "one", title: "Chat one", agent_id: "agent-1", message_id: "m-1",
      snippet: "plan the launch", updated_at: "2026-10-28T03:00:00Z", archived: backend.archived,
    }],
    next_cursor: null,
  }));
});

afterEach(() => onlineManager.setOnline(true));

describe("ChatSearch review fixes", () => {
  it("shows the disconnected state, not Searching, when the query is paused offline", async () => {
    onlineManager.setOnline(false);
    const queryClient = renderSearch();
    await userEvent.type(screen.getByRole("searchbox"), "launch");
    await waitFor(() =>
      expect(queryClient.getQueryState(["chat", "ws-1", "search", "launch"])?.fetchStatus).toBe("paused"),
    );
    expect(backend.searchChats).not.toHaveBeenCalled();
    expect(await screen.findByText(enChat.search.disconnected)).toBeTruthy();
    expect(screen.queryByText(enChat.search.searching)).toBeNull();
  });

  it("resumes and lists hits when the connection returns", async () => {
    onlineManager.setOnline(false);
    renderSearch();
    await userEvent.type(screen.getByRole("searchbox"), "launch");
    await screen.findByText(enChat.search.disconnected);
    act(() => onlineManager.setOnline(true));
    expect(await screen.findByRole("list", { name: enChat.search.results_label })).toBeTruthy();
  });

  it("Escape from a focused result clears the query and restores the list", async () => {
    renderSearch();
    await userEvent.type(screen.getByRole("searchbox"), "launch");
    const result = await screen.findByRole("link", { name: /Chat one/ });
    result.focus();
    const seen = vi.fn();
    document.addEventListener("keydown", seen);
    await userEvent.keyboard("{Escape}");
    document.removeEventListener("keydown", seen);
    expect(useChatSearchStore.getState().queries.page ?? "").toBe("");
    expect(screen.getByText("Normal list")).toBeTruthy();
    // The event stopped before anything that would dismiss a popover or dialog.
    expect(seen).not.toHaveBeenCalled();
  });

  it("Escape with no query is left to the surrounding surface", async () => {
    renderSearch();
    const seen = vi.fn();
    document.addEventListener("keydown", seen);
    screen.getByRole("searchbox").focus();
    await userEvent.keyboard("{Escape}");
    document.removeEventListener("keydown", seen);
    expect(seen).toHaveBeenCalled();
  });

  it("Unarchive refreshes the hit after the server commits, and again if it fails", async () => {
    let commit!: () => void;
    backend.setArchived.mockImplementation(() => new Promise<ChatSession>((resolve) => {
      commit = () => {
        backend.archived = false;
        resolve({ ...archivedSession, status: "active" });
      };
    }));
    const queryClient = renderSearch();
    queryClient.setQueryData(chatKeys.sessions("ws-1"), [archivedSession]);
    await userEvent.type(screen.getByRole("searchbox"), "launch");
    await userEvent.click(await screen.findByRole("button", { name: enChat.list.unarchive }));
    await waitFor(() => expect(backend.setArchived).toHaveBeenCalled());
    await act(async () => commit());
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    await waitFor(() => expect(screen.queryByRole("button", { name: enChat.list.unarchive })).toBeNull());
  });

  it("refreshes the hits after a failed Unarchive too", async () => {
    backend.setArchived.mockRejectedValue(new Error("boom"));
    const queryClient = renderSearch();
    queryClient.setQueryData(chatKeys.sessions("ws-1"), [archivedSession]);
    await userEvent.type(screen.getByRole("searchbox"), "launch");
    await userEvent.click(await screen.findByRole("button", { name: enChat.list.unarchive }));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    // First load, then one refresh after the failed mutation settled.
    await waitFor(() => expect(backend.searchChats.mock.calls.length).toBeGreaterThanOrEqual(2));
    expect(screen.getByRole("button", { name: enChat.list.unarchive })).toBeTruthy();
  });
});
