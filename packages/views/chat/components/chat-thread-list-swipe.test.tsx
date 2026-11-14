import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Agent, ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import enIssues from "../../locales/en/issues.json";
import { useChatStore } from "@multica/core/chat";

// Swipe, long-press and the always-visible ⋯ on the DM list (CHE-5561). The
// archive path runs through the real useChatSessionActions so the Undo toast is
// the shipped one; only the network mutations and sonner are faked.

const setArchived = vi.hoisted(() => vi.fn());
const setPinned = vi.hoisted(() => vi.fn());
const markRead = vi.hoisted(() => vi.fn());
const markUnread = vi.hoisted(() => vi.fn());
const toastShow = vi.hoisted(() => Object.assign(vi.fn(), { error: vi.fn() }));

vi.mock("sonner", () => ({ toast: toastShow }));
vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/paths")>()),
  useWorkspaceSlug: () => "test-ws",
}));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => <span /> }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/agents", () => ({ useWorkspacePresenceMap: () => ({ byAgent: new Map() }) }));
vi.mock("@multica/core/api", () => ({ api: { cancelTaskById: vi.fn() } }));
vi.mock("@multica/core/chat", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/chat")>();
  const data = new Map<string, string>();
  actual.registerChatStore(
    actual.createChatStore({
      storage: {
        getItem: (k) => data.get(k) ?? null,
        setItem: (k, v) => void data.set(k, v),
        removeItem: (k) => void data.delete(k),
      },
    }),
  );
  return actual;
});
vi.mock("@multica/core/chat/mutations", () => ({
  useDeleteChatSession: () => ({ mutate: vi.fn(), isPending: false }),
  useSetChatSessionPinned: () => ({ mutate: setPinned, mutateAsync: setPinned, isPending: false }),
  useMarkChatSessionRead: () => ({ mutate: markRead, mutateAsync: markRead, isPending: false }),
  useUpdateChatSession: () => ({ mutate: vi.fn(), isPending: false }),
  useMarkChatSessionUnread: () => ({ mutate: markUnread, mutateAsync: markUnread, isPending: false }),
  useSetChatSessionArchived: () => ({ mutate: setArchived, mutateAsync: setArchived, isPending: false }),
}));
vi.mock("@multica/core/chat/attention-mutations", () => {
  const idle = () => ({ mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false });
  return {
    useChatAttentionXong: idle,
    useChatAttentionLater: idle,
    useChatAttentionXongAll: idle,
    useChatAttentionUndoXongAll: idle,
  };
});
vi.mock("@tanstack/react-query", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-query")>();
  return {
    ...actual,
    useQuery: (options: { queryKey: readonly unknown[] }) =>
      options.queryKey.includes("open-decisions")
        ? { data: [], isPending: false, isError: false, refetch: vi.fn() }
        : { data: { tasks: [] } },
    useQueryClient: () => ({ setQueryData: vi.fn(), invalidateQueries: vi.fn() }),
  };
});

import { ChatThreadList } from "./chat-thread-list";
import { useChatSessionActions } from "./use-chat-session-actions";

function makeSession(id: string, overrides: Partial<ChatSession> = {}): ChatSession {
  return {
    id,
    workspace_id: "ws-1",
    agent_id: "agent-1",
    creator_id: "user-1",
    title: `Chat ${id}`,
    status: "active",
    has_unread: false,
    unread_count: 0,
    last_message: null,
    pinned: false,
    created_at: new Date(0).toISOString(),
    updated_at: "2026-07-08T03:00:00Z",
    ...overrides,
  };
}

const agent = { id: "agent-1", name: "Alpha" } as unknown as Agent;
const touch = { pointerId: 1, pointerType: "touch", isPrimary: true };

function Harness({
  sessions,
  onEnterSelectMode,
}: {
  sessions: ChatSession[];
  onEnterSelectMode?: (s: ChatSession) => void;
}) {
  const actions = useChatSessionActions();
  return (
    <ChatThreadList
      sessions={sessions}
      agents={[agent]}
      activeSessionId={null}
      onSelectSession={vi.fn()}
      onArchive={(s) => void actions.archive(s)}
      onEnterSelectMode={onEnterSelectMode}
    />
  );
}

function renderList(sessions: ChatSession[], onEnterSelectMode?: (s: ChatSession) => void) {
  return render(
    <I18nProvider locale="en" resources={{ en: { chat: enChat, issues: enIssues } }}>
      <Harness sessions={sessions} onEnterSelectMode={onEnterSelectMode} />
    </I18nProvider>,
  );
}

function row(id: string) {
  return screen.getByText(`Chat ${id}`).closest("[data-testid='chat-thread-row']")!;
}

// The swipe tray is a sibling of the row; the row's own hover strip and ⋯ carry
// the same names, so query the tray only.
function tray(id: string) {
  const r = row(id);
  const root = r.parentElement!.parentElement!; // row → translating div → SwipeRow root
  return within(root as HTMLElement).getAllByRole("button").filter((b) => !r.contains(b));
}
function trayButton(id: string, name: string) {
  return tray(id).find((b) => b.getAttribute("aria-label") === name)!;
}

function drag(el: Element, dx: number) {
  fireEvent.pointerDown(el, { ...touch, clientX: 300, clientY: 20 });
  fireEvent.pointerMove(el, { ...touch, clientX: 300 + dx / 2, clientY: 20 });
  fireEvent.pointerMove(el, { ...touch, clientX: 300 + dx, clientY: 20 });
  fireEvent.pointerUp(el, { ...touch, clientX: 300 + dx, clientY: 20 });
}

beforeEach(() => {
  vi.useFakeTimers();
  useChatStore.setState({ activeSessionId: null, listView: "main" });
  for (const m of [setArchived, setPinned, markRead, markUnread, toastShow]) m.mockReset();
  setArchived.mockResolvedValue(undefined);
  markUnread.mockResolvedValue(undefined);
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("DM list swipe", () => {
  it("left drag reveals Pin | Archive | More", () => {
    renderList([makeSession("s1")]);
    drag(row("s1"), -120);
    expect(tray("s1").map((b) => b.getAttribute("aria-label"))).toEqual(["Pin", "Archive", "More"]);
  });

  it("right drag offers Mark as seen on an unseen DM and Mark as unseen on a seen one", () => {
    renderList([
      makeSession("s1", { has_unread: true, unread_count: 2 }),
      makeSession("s2", { updated_at: "2026-07-08T02:00:00Z" }),
    ]);
    drag(row("s1"), 120);
    fireEvent.click(trayButton("s1", "Mark as seen"));
    expect(markRead).toHaveBeenCalledWith("s1");
    drag(row("s2"), 120);
    fireEvent.click(trayButton("s2", "Mark as unseen"));
    expect(markUnread).toHaveBeenCalledWith("s2");
  });

  it("More from the tray opens the row menu", () => {
    renderList([makeSession("s1")]);
    drag(row("s1"), -120);
    fireEvent.click(trayButton("s1", "More"));
    expect(screen.getByRole("menuitem", { name: /^Archive/ })).toBeInTheDocument();
  });

  it("a full left swipe archives and shows an Undo toast that restores the DM", async () => {
    renderList([makeSession("s1")]);
    drag(row("s1"), -330);
    await act(async () => void (await vi.advanceTimersByTimeAsync(0)));
    expect(setArchived).toHaveBeenCalledWith({ sessionId: "s1", archived: true });
    const [, options] = toastShow.mock.calls[0]!;
    expect(options.action.label).toBe("Undo");
    expect(options.duration).toBe(5000);
    await act(async () => void options.action.onClick());
    expect(setArchived).toHaveBeenLastCalledWith({ sessionId: "s1", archived: false });
  });

  it("when the toast expires without Undo the DM stays archived", async () => {
    renderList([makeSession("s1")]);
    drag(row("s1"), -330);
    await act(async () => void (await vi.advanceTimersByTimeAsync(6000)));
    expect(setArchived).toHaveBeenCalledTimes(1);
    expect(setArchived).toHaveBeenCalledWith({ sessionId: "s1", archived: true });
  });

  it("an archived DM has no full-swipe archive; its tray offers Unarchive", () => {
    useChatStore.setState({ listView: "archived" });
    renderList([makeSession("s1", { status: "archived" })]);
    drag(row("s1"), -330);
    expect(tray("s1").map((b) => b.getAttribute("aria-label"))).toEqual(["Unarchive", "More"]);
    expect(setArchived).not.toHaveBeenCalled();
  });
});

describe("DM list long press", () => {
  it("enters the select seam once after 500 ms, never selecting the DM", () => {
    const seam = vi.fn();
    renderList([makeSession("s1")], seam);
    fireEvent.pointerDown(row("s1"), { ...touch, clientX: 300, clientY: 20 });
    act(() => void vi.advanceTimersByTime(500));
    fireEvent.pointerUp(row("s1"), { ...touch, clientX: 300, clientY: 20 });
    act(() => void vi.advanceTimersByTime(2000));
    expect(seam).toHaveBeenCalledTimes(1);
    expect(seam.mock.calls[0]![0]).toMatchObject({ id: "s1" });
  });

  it("movement cancels it", () => {
    const seam = vi.fn();
    renderList([makeSession("s1")], seam);
    fireEvent.pointerDown(row("s1"), { ...touch, clientX: 300, clientY: 20 });
    fireEvent.pointerMove(row("s1"), { ...touch, clientX: 270, clientY: 20 });
    act(() => void vi.advanceTimersByTime(1000));
    expect(seam).not.toHaveBeenCalled();
  });
});

describe("DM list ⋯ trigger", () => {
  it("has a 44x44 minimum hit area off hover-capable pointers", () => {
    renderList([makeSession("s1")]);
    const trigger = within(row("s1") as HTMLElement).getByRole("button", { name: "Chat actions" });
    expect(trigger.className).toMatch(/\bsize-11\b/);
    expect(trigger.className).toMatch(/hover:hover\)\]:invisible/);
  });
});
