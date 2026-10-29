import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useChatSearchStore } from "@multica/core/chat/search";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Agent, ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import enCommon from "../../locales/en/common.json";
import enIssues from "../../locales/en/issues.json";

vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => null }));
vi.mock("@multica/core/hooks", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/hooks")>()),
  useWorkspaceId: () => "ws-1",
}));
const searchChats = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/api")>();
  return {
    ...actual,
    api: { ...actual.api, getPendingChatTasks: vi.fn(async () => ({ tasks: [] })), searchChats },
  };
});

import { SessionDropdown } from "./chat-window";

function makeSession(over: Partial<ChatSession> & Pick<ChatSession, "id">): ChatSession {
  return {
    workspace_id: "ws-1",
    agent_id: "agent-1",
    creator_id: "u1",
    title: `Chat ${over.id}`,
    status: "active",
    has_unread: false,
    unread_count: 0,
    last_message: null,
    pinned: false,
    created_at: new Date(0).toISOString(),
    updated_at: new Date(0).toISOString(),
    ...over,
  };
}

const agent = { id: "agent-1", name: "Alpha" } as unknown as Agent;
const one = makeSession({ id: "one" });
const pinned = makeSession({ id: "two", pinned: true });

function setup(sessions: ChatSession[] = [one]) {
  const handlers = {
    onSelectSession: vi.fn(),
    onArchiveSession: vi.fn(),
    onTogglePin: vi.fn(),
    onOpenStatistics: vi.fn(),
    onUnarchiveSession: vi.fn(),
    onOpenMessageHit: vi.fn(),
  };
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <I18nProvider locale="en" resources={{ en: { chat: enChat, common: enCommon, issues: enIssues } }}>
        <SessionDropdown
          sessions={sessions}
          agents={[agent]}
          activeSessionId={sessions[0]?.id ?? null}
          renameRequest={0}
          {...handlers}
        />
      </I18nProvider>
    </QueryClientProvider>,
  );
  fireEvent.click(screen.getByText(sessions[0]!.title!));
  return handlers;
}

function openRowMenu(index: number) {
  fireEvent.click(
    screen.getAllByRole("button", { name: enChat.session_history.row_actions_aria })[index]!,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  useChatSearchStore.setState({ queries: {} });
});

describe("floating window session menu", () => {
  it("lists Statistics, Pin, Rename and Archive in the same order as the page menu", () => {
    setup();
    openRowMenu(0);
    const labels = screen.getAllByRole("menuitem").map((item) => item.textContent?.trim());
    expect(labels).toEqual([
      enChat.header.statistics,
      enChat.list.pin,
      enChat.session_history.row_rename_aria,
      enChat.list.archive,
    ]);
  });

  it("opens statistics for the row it belongs to", async () => {
    const { onOpenStatistics } = setup([one, pinned]);
    openRowMenu(1);
    fireEvent.click(await screen.findByRole("menuitem", { name: enChat.header.statistics }));
    expect(onOpenStatistics).toHaveBeenCalledWith(pinned);
  });

  it("offers Unpin for a pinned DM and hands the toggle to the owner", async () => {
    const { onTogglePin } = setup([pinned]);
    openRowMenu(0);
    fireEvent.click(await screen.findByRole("menuitem", { name: enChat.list.unpin }));
    expect(onTogglePin).toHaveBeenCalledWith(pinned);
  });

  it("hands Archive to the owner, which advances the selection after the server accepted it", async () => {
    const { onArchiveSession, onSelectSession } = setup();
    openRowMenu(0);
    fireEvent.click(await screen.findByRole("menuitem", { name: enChat.list.archive }));
    expect(onArchiveSession).toHaveBeenCalledWith(one);
    expect(onSelectSession).not.toHaveBeenCalled();
  });
});

describe("floating window history search", () => {
  const hit = (over: Record<string, unknown>) => ({
    session_id: "one", title: "Chat one", agent_id: "agent-1", message_id: "m-1",
    snippet: "plan the launch", updated_at: "2026-10-28T03:00:00Z", archived: false, ...over,
  });
  const field = () => screen.getByRole("searchbox", { name: enChat.search.label });

  it("puts the field first in the popover and swaps the history for hits", async () => {
    searchChats.mockResolvedValue({ results: [hit({})], next_cursor: null });
    setup([one, pinned]);
    await userEvent.type(field(), "launch");
    const list = await screen.findByRole("list", { name: enChat.search.results_label });
    expect(list.querySelectorAll("li")).toHaveLength(1);
    expect(screen.queryByText("Chat two")).toBeNull();
  });

  it("opens a title hit's chat and closes the popover", async () => {
    searchChats.mockResolvedValue({ results: [hit({ message_id: null })], next_cursor: null });
    const { onSelectSession } = setup([one, pinned]);
    await userEvent.type(field(), "launch");
    fireEvent.click(await screen.findByRole("link", { name: /Chat one/ }));
    expect(onSelectSession).toHaveBeenCalledWith(one);
    expect(screen.queryByRole("searchbox")).toBeNull();
  });

  it("hands a message hit to the owner with its message, and closes the popover", async () => {
    searchChats.mockResolvedValue({ results: [hit({})], next_cursor: null });
    const { onSelectSession, onOpenMessageHit } = setup([one, pinned]);
    await userEvent.type(field(), "launch");
    fireEvent.click(await screen.findByRole("link", { name: /Chat one/ }));
    expect(onOpenMessageHit).toHaveBeenCalledWith(one, "m-1");
    expect(onSelectSession).not.toHaveBeenCalled();
    expect(screen.queryByRole("searchbox")).toBeNull();
  });

  it("Escape drops the query first and keeps the popover open", async () => {
    searchChats.mockResolvedValue({ results: [hit({})], next_cursor: null });
    setup([one, pinned]);
    await userEvent.type(field(), "launch");
    await screen.findByRole("list", { name: enChat.search.results_label });
    await userEvent.keyboard("{Escape}");
    expect((field() as HTMLInputElement).value).toBe("");
    expect(screen.getByText("Chat two")).toBeTruthy();
  });

  it("unarchives an archived hit through the owner", async () => {
    const archived = makeSession({ id: "arch", status: "archived" });
    searchChats.mockResolvedValue({ results: [hit({ session_id: "arch", title: "Chat arch", archived: true })], next_cursor: null });
    const { onUnarchiveSession } = setup([one, archived]);
    await userEvent.type(field(), "launch");
    fireEvent.click(await screen.findByRole("button", { name: enChat.list.unarchive }));
    expect(onUnarchiveSession).toHaveBeenCalledWith(archived);
  });
});
