import { describe, expect, it, vi, beforeEach } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Agent, ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import enIssues from "../../locales/en/issues.json";
import { NavigationProvider } from "../../navigation";
import { useChatStore } from "@multica/core/chat";

// --- Mocks ------------------------------------------------------------------
// The list no longer owns the archive-advance behavior — the parent (ChatPage)
// does, so it stays layout-aware and routes through the shared controller. Here
// we only assert the history-row Archive action delegates to the `onArchive`
// prop and does NOT fire the archive mutation itself. The advance semantics
// (next / previous / clear / cross-agent sync) are covered in the controller
// test, and mobile-vs-desktop in the page. setActiveSession/archiveMutate are
// kept as negative assertions: the list must not touch them for a history
// archive anymore.

const archiveMutate = vi.fn();
const pinMutate = vi.fn();
const markUnreadAsync = vi.fn().mockResolvedValue(undefined);
const updateMutate = vi.fn();
const needsYou = { rows: [] as Record<string, unknown>[] };
const navigate = vi.fn();

vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...await importOriginal<typeof import("@multica/core/paths")>(),
  useWorkspaceSlug: () => "test-ws",
}));

vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: ({ actorId }: { actorId: string }) => (
    <span data-testid={`avatar-${actorId}`} />
  ),
}));

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));

vi.mock("@multica/core/agents", () => ({
  useWorkspacePresenceMap: () => ({ byAgent: new Map() }),
}));

vi.mock("@multica/core/api", () => ({
  api: { cancelTaskById: vi.fn() },
}));

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
  useSetChatSessionPinned: () => ({ mutate: pinMutate, isPending: false }),
  useMarkChatSessionUnread: () => ({ mutate: vi.fn(), mutateAsync: markUnreadAsync, isPending: false }),
  useUpdateChatSession: () => ({ mutate: updateMutate, isPending: false }),
  useSetChatSessionArchived: () => ({ mutate: archiveMutate, isPending: false }),
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
        ? { data: needsYou.rows, isPending: false, isError: false, refetch: vi.fn() }
        : { data: { tasks: [] } },
    useQueryClient: () => ({ setQueryData: vi.fn(), invalidateQueries: vi.fn() }),
  };
});

import { ChatThreadList } from "./chat-thread-list";

const TEST_RESOURCES = { en: { chat: enChat, issues: enIssues } };

function makeSession(overrides: Partial<ChatSession> & Pick<ChatSession, "id">): ChatSession {
  return {
    workspace_id: "ws-1",
    agent_id: "agent-1",
    creator_id: "user-1",
    title: `Chat ${overrides.id}`,
    status: "active",
    has_unread: false,
    unread_count: 0,
    last_message: null,
    pinned: false,
    created_at: new Date(0).toISOString(),
    updated_at: new Date(0).toISOString(),
    ...overrides,
  };
}

const agent = { id: "agent-1", name: "Alpha" } as unknown as Agent;

// Sessions are sorted by most-recent activity (updated_at) first, pinned first.
// Give them descending timestamps so the rendered order is s1, s2, s3.
const sessions: ChatSession[] = [
  makeSession({ id: "s1", updated_at: "2026-07-08T03:00:00Z" }),
  makeSession({ id: "s2", updated_at: "2026-07-08T02:00:00Z" }),
  makeSession({ id: "s3", updated_at: "2026-07-08T01:00:00Z" }),
];

function renderList(
  activeSessionId: string | null,
  {
    onArchive = vi.fn(),
    onSelectSession = vi.fn(),
    renderedSessions = sessions,
    renderedAgents = [agent],
    loadState = undefined as "loading" | "error" | undefined,
    onRetry = undefined as (() => void) | undefined,
    onEnterSelectMode = undefined as ((session: ChatSession) => void) | undefined,
  } = {},
) {
  const { container } = render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <NavigationProvider value={{ push: navigate, replace: vi.fn(), back: vi.fn(), pathname: "/test-ws/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (path) => path }}>
      <ChatThreadList
        sessions={renderedSessions}
        agents={renderedAgents}
        activeSessionId={activeSessionId}
        onSelectSession={onSelectSession}
        onArchive={onArchive}
        loadState={loadState}
        onRetry={onRetry}
        onEnterSelectMode={onEnterSelectMode}
      />
      </NavigationProvider>
    </I18nProvider>,
  );
  return { onArchive, onSelectSession, container };
}

const ARCHIVE_LABEL = enChat.list.archive;
const ARCHIVE_LABEL_RE = new RegExp(`^${ARCHIVE_LABEL}`);

beforeEach(() => {
  useChatStore.setState({ listView: "main" });
});

describe("ChatThreadList archive delegation", () => {
  beforeEach(() => {
    needsYou.rows = [];
    navigate.mockClear();
    useChatStore.setState({ activeSessionId: null, listView: "main" });
    archiveMutate.mockClear();
  });

  // The row's ⋯ menu is the one action surface; open it for row `index`.
  const archiveViaMenu = async (index: number) => {
    fireEvent.click(screen.getAllByRole("button", { name: enChat.list.row_actions_aria })[index]!);
    fireEvent.click(await screen.findByRole("menuitem", { name: ARCHIVE_LABEL_RE }));
  };

  it("delegates the history-row Archive action to onArchive with that session", async () => {
    const { onArchive } = renderList("s2");
    // Rows render in order s1, s2, s3.
    await archiveViaMenu(1);

    expect(onArchive).toHaveBeenCalledTimes(1);
    expect(onArchive.mock.calls[0]![0]).toMatchObject({ id: "s2" });
  });

  it("passes the archived row's session even when it isn't the open one", async () => {
    const { onArchive } = renderList("s1");
    // Archive s3 while s1 is the open one — the parent decides what to do.
    await archiveViaMenu(2);

    expect(onArchive).toHaveBeenCalledTimes(1);
    expect(onArchive.mock.calls[0]![0]).toMatchObject({ id: "s3" });
  });

  it("does not flip archive status or move selection itself", async () => {
    renderList("s2");
    await archiveViaMenu(1);

    // Advance + mutation are the parent/controller's job now; the list stays out.
    expect(useChatStore.getState().activeSessionId).toBeNull();
    expect(archiveMutate).not.toHaveBeenCalled();
  });
});

describe("ChatThreadList continuation chains", () => {
  const chain: ChatSession[] = [
    makeSession({ id: "c1", title: "Chain first", status: "archived", continued_into_session_id: "c2", updated_at: "2026-07-08T01:00:00Z" }),
    makeSession({ id: "c2", title: "Chain second", status: "archived", continued_from_session_id: "c1", continued_into_session_id: "c3", updated_at: "2026-07-08T02:00:00Z" }),
    makeSession({ id: "c3", title: "Chain newest", continued_from_session_id: "c2", updated_at: "2026-07-08T03:00:00Z" }),
    makeSession({ id: "solo", title: "Archived alone", status: "archived", updated_at: "2026-07-08T00:00:00Z" }),
  ];

  it("lists one row per chain: the newest DM; archived predecessors are not rows", () => {
    renderList(null, { renderedSessions: chain });
    expect(screen.getByText("Chain newest")).toBeInTheDocument();
    expect(screen.queryByText("Chain first")).not.toBeInTheDocument();
    expect(screen.queryByText("Chain second")).not.toBeInTheDocument();
    // The Archived entry counts only the unrelated archived DM.
    fireEvent.click(screen.getByRole("button", { name: /^Archived/ }));
    expect(screen.getByText("Archived alone")).toBeInTheDocument();
    expect(screen.queryByText("Chain first")).not.toBeInTheDocument();
    expect(screen.queryByText("Chain second")).not.toBeInTheDocument();
  });

  it("keeps an archived DM whose successor is not in the list", () => {
    const orphan = makeSession({ id: "o1", title: "Orphan source", status: "archived", continued_into_session_id: "gone" });
    renderList(null, { renderedSessions: [orphan] });
    fireEvent.click(screen.getByRole("button", { name: /^Archived/ }));
    expect(screen.getByText("Orphan source")).toBeInTheDocument();
  });
});

describe("ChatThreadList no_response preview (MUL-4351)", () => {
  it("shows a localized 'no text reply' preview instead of the fallback body", () => {
    const session = makeSession({
      id: "nr1",
      last_message: {
        content: "The agent finished this turn without a text reply.",
        role: "assistant",
        created_at: "2026-07-08T03:00:00Z",
        message_kind: "no_response",
      },
    });
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ChatThreadList
          sessions={[session]}
          agents={[agent]}
          activeSessionId={null}
          onSelectSession={vi.fn()}
          onArchive={vi.fn()}
        />
      </I18nProvider>,
    );
    expect(screen.getByText(enChat.list.no_response_preview)).toBeInTheDocument();
    // The stored English fallback body must not leak into the preview.
    expect(
      screen.queryByText("The agent finished this turn without a text reply."),
    ).not.toBeInTheDocument();
  });
});

describe("ChatThreadList agent identity", () => {
  it("shows and bounds the resolved agent name without replacing the preview", () => {
    const longName = "A very long agent name that must not take over the conversation row";
    const longNameAgent = { id: "agent-1", name: longName } as unknown as Agent;
    const session = makeSession({
      id: "agent-name",
      last_message: {
        content: "Latest project update",
        role: "assistant",
        created_at: "2026-07-08T03:00:00Z",
      },
    });

    renderList(null, {
      renderedSessions: [session],
      renderedAgents: [longNameAgent],
    });

    const name = screen.getByText(longName);
    expect(name.className).toContain("max-w-[40%]");
    expect(name.className).toContain("truncate");
    expect(screen.getByText("Latest project update")).toBeInTheDocument();
  });

  it("keeps the existing preview when the session agent is unavailable", () => {
    const session = makeSession({
      id: "missing-agent",
      agent_id: "missing-agent-id",
      last_message: {
        content: "Still identifiable by its preview",
        role: "assistant",
        created_at: "2026-07-08T03:00:00Z",
      },
    });

    renderList(null, { renderedSessions: [session] });

    expect(screen.queryByText("Alpha")).not.toBeInTheDocument();
    expect(screen.getByText("Still identifiable by its preview")).toBeInTheDocument();
  });
});

// Touch has no hover, so the hover strip's actions also live in a per-row menu.
describe("ChatThreadList compact row menu", () => {
  beforeEach(() => {
    useChatStore.setState({ activeSessionId: null, listView: "main" });
    archiveMutate.mockClear();
  });

  const openRowMenu = (index: number) =>
    fireEvent.click(
      screen.getAllByRole("button", { name: enChat.list.row_actions_aria })[index]!,
    );

  it("archives the row it belongs to", async () => {
    const { onArchive } = renderList("s1");

    openRowMenu(1);
    fireEvent.click(await screen.findByRole("menuitem", { name: ARCHIVE_LABEL_RE }));

    expect(onArchive).toHaveBeenCalledTimes(1);
    expect(onArchive.mock.calls[0]![0]).toMatchObject({ id: "s2" });
  });

  it("offers the pin toggle", async () => {
    renderList(null);

    openRowMenu(0);

    expect(await screen.findByRole("menuitem", { name: /^Pin/ })).toBeInTheDocument();
  });

  it("does not select the row when the menu opens", () => {
    const { onSelectSession } = renderList(null);

    openRowMenu(0);

    expect(onSelectSession).not.toHaveBeenCalled();
  });

  it("stays reachable on a wide viewport that cannot hover", () => {
    // Visibility switches on hover capability, never width: gating on width
    // strands a landscape phone, where the trigger would wait for a hover that
    // never arrives.
    renderList(null);

    const trigger = screen.getAllByRole("button", {
      name: enChat.list.row_actions_aria,
    })[0]!;

    expect(trigger.className).toContain("[@media(hover:hover)]:invisible");
    expect(trigger.className).toContain("[@media(hover:hover)]:group-hover/row:visible");
    expect(trigger.className).toContain("[@media(hover:hover)]:group-focus-within/row:visible");
    expect(trigger.className).not.toMatch(/(^|\s)(sm|md|lg|xl|2xl):/);
  });
});

describe("ChatThreadList row keyboard semantics", () => {
  it("selects the row on Enter", () => {
    const { onSelectSession } = renderList(null);

    const row = screen.getByText("Chat s1").closest("[tabindex]")!;
    fireEvent.keyDown(row, { key: "Enter" });

    expect(onSelectSession).toHaveBeenCalledTimes(1);
  });

  it("leaves Enter pressed inside a row control to that control", () => {
    // Opening the action menu with the keyboard must not also select the row.
    const { onSelectSession } = renderList(null);

    fireEvent.keyDown(
      screen.getAllByRole("button", { name: enChat.list.row_actions_aria })[0]!,
      { key: "Enter" },
    );

    expect(onSelectSession).not.toHaveBeenCalled();
  });
});

// CHE-5511 slice 1: filter chips replace the sticky Needs-you block.
describe("ChatThreadList filter chips", () => {
  const set: ChatSession[] = [
    makeSession({ id: "active", title: "Active", updated_at: "2026-07-08T09:00:00Z" }),
    makeSession({ id: "arch", title: "Archived one", status: "archived", updated_at: "2026-07-08T08:00:00Z" }),
    makeSession({ id: "pin", title: "Pinned one", pinned: true, updated_at: "2026-07-08T07:00:00Z" }),
    makeSession({ id: "unseen", title: "Unseen one", has_unread: true, unread_count: 1, updated_at: "2026-07-08T06:00:00Z" }),
    makeSession({ id: "needs", title: "Needs one", stale_level: "red", attention_source: "ask", attention_since: "2026-07-08T01:00:00Z", updated_at: "2026-07-08T05:00:00Z" }),
    makeSession({ id: "dec", title: "Decision one", updated_at: "2026-07-08T04:00:00Z" }),
  ];
  const titles = () =>
    screen.queryAllByTestId("chat-thread-row").map((r) => r.querySelector("[title]")?.textContent);
  const chip = (name: string | RegExp) => screen.getByRole("button", { name });

  beforeEach(() => {
    useChatStore.setState({ listView: "main" });
    needsYou.rows = [{ id: "d1", chat_session_id: "dec", issue_identifier: "CHE-1", ask: "x", created_at: "2026-07-08T00:00:00Z", escalation_class: "spend" }];
  });

  it("defaults to Main: every DM that is not archived", () => {
    renderList(null, { renderedSessions: set });
    expect(chip(/^Main/)).toHaveAttribute("aria-pressed", "true");
    expect(titles()).toEqual(["Pinned one", "Active", "Unseen one", "Needs one", "Decision one"]);
  });

  it("All = Main + Archived; Archived = archived only", () => {
    renderList(null, { renderedSessions: set });
    fireEvent.click(chip(/^All/));
    expect(titles()).toHaveLength(6);
    expect(titles()).toContain("Archived one");
    fireEvent.click(chip(/^Archived/));
    expect(titles()).toEqual(["Archived one"]);
  });

  it("Decisions, Unseen and Pinned each show exactly their set", () => {
    renderList(null, { renderedSessions: set });
    fireEvent.click(chip(/^Decisions/));
    expect(titles()).toEqual(["Decision one"]);
    fireEvent.click(chip(/^Unseen/));
    expect(titles()).toEqual(["Unseen one"]);
    fireEvent.click(chip(/^Pinned/));
    expect(titles()).toEqual(["Pinned one"]);
    fireEvent.click(chip(/^Pinned/));
    expect(titles()).toEqual(["Active", "Unseen one", "Needs one", "Decision one"]);
  });

  it("Waiting on you shows the attention rows, not a sticky block", () => {
    renderList(null, { renderedSessions: set });
    expect(screen.queryByRole("heading", { name: /Needs you/ })).not.toBeInTheDocument();
    expect(screen.queryByText("Open decisions")).not.toBeInTheDocument();
    fireEvent.click(chip(/^Waiting on you/));
    expect(screen.getAllByTestId("attention-row").map((r) => r.getAttribute("data-session-id"))).toEqual(["needs"]);
    const block = screen.getByRole("region", { name: "Needs you" });
    expect(block.className).not.toContain("sticky");
    expect(block.querySelector(".max-h-64, .overflow-y-auto")).toBeNull();
  });

  it("chips carry counts once known", () => {
    renderList(null, { renderedSessions: set });
    expect(chip("Decisions, 1")).toBeInTheDocument();
    expect(chip("Waiting on you, 1")).toBeInTheDocument();
    expect(chip("Unseen, 1")).toBeInTheDocument();
    expect(chip("Archived, 1")).toBeInTheDocument();
  });

  it("keeps the chip between list and a DM, and the list view after remount", () => {
    const first = render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ChatThreadList sessions={set} agents={[agent]} activeSessionId={null} onSelectSession={vi.fn()} onArchive={vi.fn()} />
      </I18nProvider>,
    );
    fireEvent.click(chip(/^Unseen/));
    first.unmount(); // list → DM (list unmounts on mobile)
    renderList(null, { renderedSessions: set });
    expect(chip(/^Unseen/)).toHaveAttribute("aria-pressed", "true");
    expect(titles()).toEqual(["Unseen one"]);
  });

  it("shows an empty state instead of a blank list", () => {
    renderList(null, { renderedSessions: [] });
    expect(screen.getByText("No chats yet")).toBeInTheDocument();
    cleanup();
    renderList(null, { renderedSessions: [set[0]!] });
    fireEvent.click(chip(/^Decisions/));
    expect(screen.getByText("No chats in this view")).toBeInTheDocument();
  });

  it("shows skeleton rows while loading, hiding unknown counts", () => {
    renderList(null, { renderedSessions: [], loadState: "loading" });
    expect(screen.getByRole("status", { name: "Loading chats…" })).toBeInTheDocument();
    expect(chip("Decisions")).toBeInTheDocument();
    expect(screen.queryByText("No chats yet")).not.toBeInTheDocument();
  });

  it("shows the error with Retry that calls onRetry", () => {
    const onRetry = vi.fn();
    renderList(null, { renderedSessions: [], loadState: "error", onRetry });
    expect(screen.getByRole("alert")).toHaveTextContent("Couldn't load chats");
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(onRetry).toHaveBeenCalledOnce();
  });

  it("says so when offline and keeps the saved list", () => {
    const online = vi.spyOn(window.navigator, "onLine", "get").mockReturnValue(false);
    renderList(null, { renderedSessions: set });
    expect(screen.getByRole("status", { name: "You're offline" })).toBeInTheDocument();
    expect(titles()).toHaveLength(5);
    online.mockRestore();
  });
});

// CHE-5511 slice 3: desktop row actions.
describe("ChatThreadList row shortcuts and context menu", () => {
  beforeEach(() => {
    useChatStore.setState({ activeSessionId: null, listView: "main" });
    pinMutate.mockClear();
    markUnreadAsync.mockClear();
    updateMutate.mockClear();
  });

  const rowOf = (title: string) => screen.getByText(title).closest("[tabindex]") as HTMLElement;

  it("U marks the focused row unseen once", async () => {
    renderList(null);
    fireEvent.keyDown(rowOf("Chat s2"), { key: "u" });

    await waitFor(() => expect(markUnreadAsync).toHaveBeenCalledTimes(1));
    expect(markUnreadAsync).toHaveBeenCalledWith("s2");
  });

  it("P pins the focused row once", () => {
    renderList(null);
    fireEvent.keyDown(rowOf("Chat s2"), { key: "p" });

    expect(pinMutate).toHaveBeenCalledTimes(1);
    expect(pinMutate).toHaveBeenCalledWith({ sessionId: "s2", pinned: true });
  });

  it("E archives the focused row once", () => {
    const { onArchive } = renderList(null);
    fireEvent.keyDown(rowOf("Chat s3"), { key: "e" });

    expect(onArchive).toHaveBeenCalledTimes(1);
    expect(onArchive.mock.calls[0]![0]).toMatchObject({ id: "s3" });
  });

  it("R opens an inline rename that saves the new title", () => {
    renderList(null);
    fireEvent.keyDown(rowOf("Chat s1"), { key: "r" });

    const input = screen.getByRole("textbox", { name: enChat.session_history.row_rename_aria });
    fireEvent.change(input, { target: { value: "Renamed" } });
    fireEvent.keyDown(input, { key: "Enter" });

    expect(updateMutate).toHaveBeenCalledTimes(1);
    expect(updateMutate.mock.calls[0]![0]).toEqual({ sessionId: "s1", title: "Renamed" });
  });

  it("X raises the select event with the row's session", () => {
    const onEnterSelectMode = vi.fn();
    renderList(null, { onEnterSelectMode });
    fireEvent.keyDown(rowOf("Chat s2"), { key: "x" });

    expect(onEnterSelectMode).toHaveBeenCalledTimes(1);
    expect(onEnterSelectMode.mock.calls[0]![0]).toMatchObject({ id: "s2" });
  });

  it("offers no select item until the host wires select mode", async () => {
    renderList(null);
    fireEvent.click(screen.getAllByRole("button", { name: enChat.list.row_actions_aria })[0]!);

    await screen.findByRole("menuitem", { name: new RegExp(`^${enChat.list.rename}`) });
    expect(screen.queryByRole("menuitem", { name: new RegExp(`^${enChat.list.select}`) })).toBeNull();
  });

  it("does not fire while typing in an input or with a modifier held", () => {
    const { onArchive } = renderList(null);
    const row = rowOf("Chat s1");
    const field = document.createElement("input");
    row.appendChild(field);

    fireEvent.keyDown(field, { key: "e" });
    fireEvent.keyDown(row, { key: "e", metaKey: true });
    fireEvent.keyDown(row, { key: "p", ctrlKey: true });

    expect(onArchive).not.toHaveBeenCalled();
    expect(pinMutate).not.toHaveBeenCalled();
  });

  it("ignores shortcut keys pressed inside the open ⋯ menu", async () => {
    const { onArchive } = renderList(null);
    fireEvent.click(screen.getAllByRole("button", { name: enChat.list.row_actions_aria })[0]!);
    const item = await screen.findByRole("menuitem", { name: ARCHIVE_LABEL_RE });

    fireEvent.keyDown(item, { key: "e" });

    expect(onArchive).not.toHaveBeenCalled();
  });

  it("right-click opens the same items the ⋯ menu lists", async () => {
    renderList(null, { onEnterSelectMode: vi.fn() });

    fireEvent.click(screen.getAllByRole("button", { name: enChat.list.row_actions_aria })[0]!);
    const viaButton = (await screen.findAllByRole("menuitem")).map((i) => i.textContent);
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    await waitFor(() => expect(screen.queryAllByRole("menuitem")).toHaveLength(0));

    fireEvent.contextMenu(rowOf("Chat s1"));
    const viaRightClick = (await screen.findAllByRole("menuitem")).map((i) => i.textContent);

    expect(viaRightClick).toEqual(viaButton);
    expect(viaRightClick.length).toBeGreaterThanOrEqual(5);
  });

  it("has no hover strip: the ⋯ trigger is the only row control", () => {
    renderList(null);

    expect(screen.queryByTestId("chat-thread-row-archive-item")).toBeNull();
  });
});
