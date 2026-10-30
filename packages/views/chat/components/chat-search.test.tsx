import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { ApiError } from "@multica/core/api/client";
import { useChatSearchStore } from "@multica/core/chat/search";
import type { Agent, ChatSearchHit, ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";

const searchChats = vi.fn();

vi.mock("@multica/core/api", () => ({ api: { searchChats: (...a: unknown[]) => searchChats(...a) } }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: ({ actorId }: { actorId: string }) => <span data-testid={`avatar-${actorId}`} />,
}));

import { ChatSearch } from "./chat-search";

const agent = { id: "agent-1", name: "Alpha" } as unknown as Agent;

function hit(overrides: Partial<ChatSearchHit> & Pick<ChatSearchHit, "session_id">): ChatSearchHit {
  return {
    title: `Chat ${overrides.session_id}`,
    agent_id: "agent-1",
    message_id: "m-1",
    snippet: "we should plan the launch next week",
    updated_at: "2026-10-28T03:00:00Z",
    archived: false,
    ...overrides,
  };
}

function session(id: string, status: ChatSession["status"] = "active"): ChatSession {
  return {
    id, workspace_id: "ws-1", agent_id: "agent-1", creator_id: "u", title: `Chat ${id}`, status,
    has_unread: false, unread_count: 0, last_message: null, pinned: false,
    created_at: "2026-10-01T00:00:00Z", updated_at: "2026-10-28T03:00:00Z",
  };
}

const onOpenHit = vi.fn();
const onUnarchive = vi.fn();
const onTogglePin = vi.fn();
const onArchive = vi.fn();

function renderSearch(sessions: ChatSession[] = [session("s1"), session("s2", "archived")], activeSessionId: string | null = null) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
        <ChatSearch
          scope="page"
          sessions={sessions}
          agents={[agent]}
          activeSessionId={activeSessionId}
          onOpenHit={onOpenHit}
          onUnarchive={onUnarchive}
          onTogglePin={onTogglePin}
          onArchive={onArchive}
        >
          <div data-testid="normal-list">normal list</div>
        </ChatSearch>
      </I18nProvider>
    </QueryClientProvider>,
  );
}

const FIELD = enChat.search.label;

beforeEach(() => {
  searchChats.mockReset();
  onOpenHit.mockReset();
  onUnarchive.mockReset();
  useChatSearchStore.setState({ queries: {} });
});

describe("ChatSearch", () => {
  it("shows the normal list until a query is typed, then replaces it with a labelled result list", async () => {
    searchChats.mockResolvedValue({
      results: [hit({ session_id: "s1" })],
      next_cursor: null,
    });
    renderSearch();
    expect(screen.getByTestId("normal-list")).toBeTruthy();

    await userEvent.type(screen.getByRole("searchbox", { name: FIELD }), "launch");

    const list = await screen.findByRole("list", { name: enChat.search.results_label });
    expect(screen.queryByTestId("normal-list")).toBeNull();
    expect(within(list).getAllByRole("listitem")).toHaveLength(1);
    expect(await screen.findByRole("status")).toHaveTextContent("1 result");
    expect(searchChats).toHaveBeenCalledWith({ q: "launch", limit: 20, cursor: null });
  });

  it("marks the match in the snippet with a text mark", async () => {
    searchChats.mockResolvedValue({ results: [hit({ session_id: "s1" })], next_cursor: null });
    renderSearch();
    await userEvent.type(screen.getByRole("searchbox", { name: FIELD }), "launch");
    const mark = await screen.findByText("launch", { selector: "mark" });
    expect(mark.parentElement).toHaveTextContent("we should plan the launch next week");
  });

  it("lists title hits before message hits", async () => {
    searchChats.mockResolvedValue({
      results: [
        hit({ session_id: "s1", message_id: "m-1", title: "Message hit" }),
        hit({ session_id: "s2", message_id: null, title: "Launch notes", snippet: "" }),
      ],
      next_cursor: null,
    });
    renderSearch();
    await userEvent.type(screen.getByRole("searchbox", { name: FIELD }), "launch");
    const items = await screen.findAllByRole("listitem");
    expect(items[0]).toHaveTextContent("Launch notes");
    expect(items[1]).toHaveTextContent("Message hit");
  });

  it("Escape clears the query and restores the normal list", async () => {
    searchChats.mockResolvedValue({ results: [hit({ session_id: "s1" })], next_cursor: null });
    renderSearch();
    const field = screen.getByRole("searchbox", { name: FIELD });
    await userEvent.type(field, "launch");
    await screen.findByRole("list", { name: enChat.search.results_label });
    await userEvent.keyboard("{Escape}");
    expect(screen.getByTestId("normal-list")).toBeTruthy();
    expect((field as HTMLInputElement).value).toBe("");
  });

  it("the Clear button restores the list and keeps focus in the field", async () => {
    searchChats.mockResolvedValue({ results: [], next_cursor: null });
    renderSearch();
    const field = screen.getByRole("searchbox", { name: FIELD });
    await userEvent.type(field, "zzz");
    await userEvent.click(screen.getByRole("button", { name: enChat.search.clear }));
    expect(screen.getByTestId("normal-list")).toBeTruthy();
    expect(document.activeElement).toBe(field);
  });

  it("opens a hit through its open link and marks the open chat with aria-current", async () => {
    searchChats.mockResolvedValue({
      results: [hit({ session_id: "s1", message_id: "m-7" })],
      next_cursor: null,
    });
    renderSearch([session("s1")], "s1");
    await userEvent.type(screen.getByRole("searchbox", { name: FIELD }), "launch");
    const link = await screen.findByRole("link", { name: /Chat s1/ });
    expect(link).toHaveAttribute("aria-current", "true");
    fireEvent.click(link);
    expect(onOpenHit).toHaveBeenCalledWith(expect.objectContaining({ session_id: "s1", message_id: "m-7" }));
  });

  it("marks an archived hit and offers Unarchive beside the open link, never Delete", async () => {
    searchChats.mockResolvedValue({
      results: [hit({ session_id: "s2", archived: true })],
      next_cursor: null,
    });
    renderSearch();
    await userEvent.type(screen.getByRole("searchbox", { name: FIELD }), "launch");
    const item = await screen.findByRole("listitem");
    expect(within(item).getByText(enChat.search.archived_badge)).toBeTruthy();
    expect(within(item).queryByRole("button", { name: /delete/i })).toBeNull();
    await userEvent.click(within(item).getByRole("button", { name: enChat.list.unarchive }));
    expect(onUnarchive).toHaveBeenCalledWith(expect.objectContaining({ id: "s2" }));
  });

  it("offers Pin and Archive on an active hit as a labelled sibling group", async () => {
    searchChats.mockResolvedValue({ results: [hit({ session_id: "s1" })], next_cursor: null });
    renderSearch();
    await userEvent.type(screen.getByRole("searchbox", { name: FIELD }), "launch");
    const item = await screen.findByRole("listitem");
    const group = within(item).getByRole("group", { name: "Actions for Chat s1" });
    await userEvent.click(within(group).getByRole("button", { name: enChat.list.pin }));
    expect(onTogglePin).toHaveBeenCalledWith(expect.objectContaining({ id: "s1" }));
    expect(within(item).getByRole("link")).not.toContainElement(group);
  });

  it("loads the next page with Show more", async () => {
    searchChats
      .mockResolvedValueOnce({ results: [hit({ session_id: "s1" })], next_cursor: "c2" })
      .mockResolvedValueOnce({ results: [hit({ session_id: "s2", title: "Second" })], next_cursor: null });
    renderSearch();
    await userEvent.type(screen.getByRole("searchbox", { name: FIELD }), "launch");
    await userEvent.click(await screen.findByRole("button", { name: enChat.search.show_more }));
    expect(await screen.findAllByRole("listitem")).toHaveLength(2);
    expect(searchChats).toHaveBeenLastCalledWith({ q: "launch", limit: 20, cursor: "c2" });
    expect(screen.queryByRole("button", { name: enChat.search.show_more })).toBeNull();
  });

  it("says so when nothing matches", async () => {
    searchChats.mockResolvedValue({ results: [], next_cursor: null });
    renderSearch();
    await userEvent.type(screen.getByRole("searchbox", { name: FIELD }), "zzz");
    expect(await screen.findByText("No chats match “zzz”")).toBeTruthy();
  });

  it.each([
    ["failed", new ApiError("boom", 500, "Internal Server Error"), enChat.search.failed],
    ["timed out", new ApiError("slow", 503, "Service Unavailable"), enChat.search.timed_out],
    ["disconnected", new TypeError("Failed to fetch"), enChat.search.disconnected],
  ])("shows the %s state with a retry", async (_name, error, text) => {
    searchChats.mockRejectedValueOnce(error);
    renderSearch();
    await userEvent.type(screen.getByRole("searchbox", { name: FIELD }), "launch");
    expect(await screen.findByText(text)).toBeTruthy();
    searchChats.mockResolvedValueOnce({ results: [hit({ session_id: "s1" })], next_cursor: null });
    await userEvent.click(screen.getByRole("button", { name: enChat.search.retry }));
    await waitFor(() => expect(screen.getByRole("list", { name: enChat.search.results_label })).toBeTruthy());
  });

  it("does not call the server for a query over 200 characters", async () => {
    renderSearch();
    fireEvent.change(screen.getByRole("searchbox", { name: FIELD }), { target: { value: "a".repeat(201) } });
    expect(await screen.findByText("Search is limited to 200 characters.")).toBeTruthy();
    expect(searchChats).not.toHaveBeenCalled();
  });

  it("keeps the query in the store so a remount restores the field and its results", async () => {
    searchChats.mockResolvedValue({ results: [hit({ session_id: "s1" })], next_cursor: null });
    const first = renderSearch();
    await userEvent.type(screen.getByRole("searchbox", { name: FIELD }), "launch");
    await screen.findByRole("list", { name: enChat.search.results_label });
    first.unmount();
    renderSearch();
    expect((screen.getByRole("searchbox", { name: FIELD }) as HTMLInputElement).value).toBe("launch");
    expect(await screen.findByRole("list", { name: enChat.search.results_label })).toBeTruthy();
  });
});
