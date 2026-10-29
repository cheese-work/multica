import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Agent, ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import enIssues from "../../locales/en/issues.json";
import { NavigationProvider } from "../../navigation";

// --- Mocks ------------------------------------------------------------------
// The list no longer owns the archive-advance behavior — the parent (ChatPage)
// does, so it stays layout-aware and routes through the shared controller. Here
// we only assert the history-row Archive action delegates to the `onArchive`
// prop and does NOT fire the archive mutation itself. The advance semantics
// (next / previous / clear / cross-agent sync) are covered in the controller
// test, and mobile-vs-desktop in the page. setActiveSession/archiveMutate are
// kept as negative assertions: the list must not touch them for a history
// archive anymore.

const setActiveSession = vi.fn();
const archiveMutate = vi.fn();
const mockPresenceByAgent = new Map<string, { availability: string }>();
const mockPendingTasks = {
  tasks: [] as Array<{
    task_id: string;
    status: string;
    chat_session_id: string;
    created_at?: string;
  }>,
};
const retryNeedsYou = vi.fn();
const needsYou = { rows: [] as Record<string, unknown>[], isPending: false, isError: false };
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
  useWorkspacePresenceMap: () => ({ byAgent: mockPresenceByAgent }),
}));

vi.mock("@multica/core/api", () => ({
  api: { cancelTaskById: vi.fn() },
}));

vi.mock("@multica/core/chat", () => ({
  useChatStore: (selector: (s: { setActiveSession: typeof setActiveSession }) => unknown) =>
    selector({ setActiveSession }),
}));

vi.mock("@multica/core/chat/mutations", () => ({
  useDeleteChatSession: () => ({ mutate: vi.fn(), isPending: false }),
  useSetChatSessionPinned: () => ({ mutate: vi.fn(), isPending: false }),
  useSetChatSessionArchived: () => ({ mutate: archiveMutate, isPending: false }),
}));

vi.mock("@tanstack/react-query", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-query")>();
  return {
    ...actual,
    useQuery: (options: { queryKey: readonly unknown[] }) =>
      options.queryKey.includes("open-decisions")
        ? { data: needsYou.rows, isPending: needsYou.isPending, isError: needsYou.isError, refetch: retryNeedsYou }
        : { data: { tasks: mockPendingTasks.tasks } },
    useQueryClient: () => ({ setQueryData: vi.fn(), invalidateQueries: vi.fn() }),
  };
});

import { ChatThreadList } from "./chat-thread-list";
import { pickStageKeys } from "./task-status-pill";

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
  } = {},
) {
  render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <NavigationProvider value={{ push: navigate, replace: vi.fn(), back: vi.fn(), pathname: "/test-ws/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (path) => path }}>
      <ChatThreadList
        sessions={renderedSessions}
        agents={renderedAgents}
        activeSessionId={activeSessionId}
        onSelectSession={onSelectSession}
        onArchive={onArchive}
      />
      </NavigationProvider>
    </I18nProvider>,
  );
  return { onArchive, onSelectSession };
}

const ARCHIVE_LABEL = enChat.list.archive;

describe("ChatThreadList archive delegation", () => {
  beforeEach(() => {
    needsYou.rows = [];
    needsYou.isPending = false;
    needsYou.isError = false;
    navigate.mockClear();
    retryNeedsYou.mockClear();
    setActiveSession.mockClear();
    archiveMutate.mockClear();
  });

  it("keeps a read-but-unanswered ask above history with a count and its card jump", () => {
    needsYou.rows = [{
      id: "request-1", issue_id: "issue-1", issue_identifier: "CHE-2405",
      asking_agent_id: "agent-1", asking_agent_name: "Alpha", escalation_class: "spend",
      ask: "**Agent did**\nChecks passed.\n\n**Decision**\nApprove the build?\n\n**Stakes if wrong**\nA slow build.", created_at: "2026-10-28T04:00:00Z",
      chat_session_id: "s1", message_id: "ask-message",
    }];
    renderList("s1");
    expect(screen.getByRole("heading", { name: /Needs you/ })).toBeInTheDocument();
    expect(screen.getByText("Approve the build?")).toBeInTheDocument();
    expect(screen.getByText("CHE-2405")).toBeInTheDocument();
    expect(screen.getByText("spend")).toBeInTheDocument();
    expect(screen.queryByText("Checks passed.")).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Approve the build/ })).toHaveAttribute(
      "href", expect.stringContaining("session=s1&message=ask-message"),
    );
    expect(needsYou.rows).toHaveLength(1);
    fireEvent.click(screen.getByRole("link", { name: /Approve the build/ }));
    expect(navigate).toHaveBeenCalledWith("/test-ws/chat?session=s1&message=ask-message");
  });

  it("shows the Needs you count even when chat history is empty", () => {
    renderList(null, { renderedSessions: [] });
    expect(screen.getByRole("heading", { name: "Needs you 0" })).toBeInTheDocument();
    expect(screen.getByText("No open asks.")).toBeInTheDocument();
  });

  it("distinguishes loading and query errors from no open asks", () => {
    needsYou.isPending = true;
    renderList(null);
    expect(screen.getByRole("status")).toHaveTextContent("Loading open asks");
    expect(screen.queryByText("No open asks.")).not.toBeInTheDocument();
  });

  it("offers retry when the open-ask query fails", () => {
    needsYou.isError = true;
    renderList(null);
    expect(screen.getByRole("alert")).toHaveTextContent("Could not load open asks.");
    expect(screen.queryByText("No open asks.")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(retryNeedsYou).toHaveBeenCalledOnce();
  });

  it("delegates the history-row Archive action to onArchive with that session", () => {
    const { onArchive } = renderList("s2");
    // Rows render in order s1, s2, s3 → the second archive button is s2's.
    const archiveButtons = screen.getAllByRole("button", { name: ARCHIVE_LABEL });
    fireEvent.click(archiveButtons[1]!);

    expect(onArchive).toHaveBeenCalledTimes(1);
    expect(onArchive.mock.calls[0]![0]).toMatchObject({ id: "s2" });
  });

  it("passes the archived row's session even when it isn't the open one", () => {
    const { onArchive } = renderList("s1");
    const archiveButtons = screen.getAllByRole("button", { name: ARCHIVE_LABEL });
    // Archive s3 while s1 is the open one — the parent decides what to do.
    fireEvent.click(archiveButtons[2]!);

    expect(onArchive).toHaveBeenCalledTimes(1);
    expect(onArchive.mock.calls[0]![0]).toMatchObject({ id: "s3" });
  });

  it("does not flip archive status or move selection itself", () => {
    renderList("s2");
    const archiveButtons = screen.getAllByRole("button", { name: ARCHIVE_LABEL });
    fireEvent.click(archiveButtons[1]!);

    // Advance + mutation are the parent/controller's job now; the list stays out.
    expect(setActiveSession).not.toHaveBeenCalled();
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
    fireEvent.click(screen.getByRole("button", { name: new RegExp(enChat.list.archived_title) }));
    expect(screen.getByText("Archived alone")).toBeInTheDocument();
    expect(screen.queryByText("Chain first")).not.toBeInTheDocument();
    expect(screen.queryByText("Chain second")).not.toBeInTheDocument();
  });

  it("keeps an archived DM whose successor is not in the list", () => {
    const orphan = makeSession({ id: "o1", title: "Orphan source", status: "archived", continued_into_session_id: "gone" });
    renderList(null, { renderedSessions: [orphan] });
    fireEvent.click(screen.getByRole("button", { name: new RegExp(enChat.list.archived_title) }));
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
    setActiveSession.mockClear();
    archiveMutate.mockClear();
  });

  const openRowMenu = (index: number) =>
    fireEvent.click(
      screen.getAllByRole("button", { name: enChat.list.row_actions_aria })[index]!,
    );

  it("archives the row it belongs to", async () => {
    const { onArchive } = renderList("s1");

    openRowMenu(1);
    fireEvent.click(await screen.findByRole("menuitem", { name: ARCHIVE_LABEL }));

    expect(onArchive).toHaveBeenCalledTimes(1);
    expect(onArchive.mock.calls[0]![0]).toMatchObject({ id: "s2" });
  });

  it("offers the same pin toggle the hover strip does", async () => {
    renderList(null);

    openRowMenu(0);

    expect(await screen.findByRole("menuitem", { name: enChat.list.pin })).toBeInTheDocument();
  });

  it("does not select the row when the menu opens", () => {
    const { onSelectSession } = renderList(null);

    openRowMenu(0);

    expect(onSelectSession).not.toHaveBeenCalled();
  });

  it("stays reachable on a wide viewport that cannot hover", () => {
    // The menu and the hover strip must both switch on hover capability. Gating
    // either on width strands a landscape phone: past `md` the menu would be
    // hidden and the strip would wait for a hover that never arrives.
    renderList(null);

    const trigger = screen.getAllByRole("button", {
      name: enChat.list.row_actions_aria,
    })[0]!;
    const strip = trigger.parentElement!.querySelector(
      "[class*='group-hover/row']",
    )!;

    expect(trigger.className).toContain("[@media(hover:hover)]:hidden");
    expect(trigger.className).not.toMatch(/(^|\s)(sm|md|lg|xl|2xl):hidden/);
    expect(strip.className).toContain("[@media(hover:hover)]:group-hover/row:flex");
    expect(strip.className).not.toMatch(/(^|\s)(sm|md|lg|xl|2xl):group-/);
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

describe("ChatThreadList shared DM run-state labels and elapsed time (CHE-1938)", () => {
  beforeEach(() => {
    mockPresenceByAgent.clear();
    mockPendingTasks.tasks = [];
    vi.useRealTimers();
  });

  const cases = [
    { status: "queued", availability: "online" as const },
    { status: "dispatched", availability: "online" as const },
    { status: "deferred", availability: "online" as const },
    { status: "waiting_local_directory", availability: "online" as const },
    { status: "running", availability: "online" as const },
    { status: "queued", availability: "offline" as const },
  ] as const;

  for (const { status, availability } of cases) {
    it(`renders the same stage label as pickStageKeys for status=${status} availability=${availability}`, () => {
      mockPresenceByAgent.set("agent-1", { availability });
      mockPendingTasks.tasks = [
        { task_id: "t-1", status, chat_session_id: "s1" },
      ];
      const expectedStageKey = pickStageKeys(status, [], availability).stageKey;
      const expectedLabel = enChat.status_pill.stages[expectedStageKey];

      const { container } = render(
        <I18nProvider locale="en" resources={TEST_RESOURCES}>
          <ChatThreadList
            sessions={[sessions[0]!]}
            agents={[agent]}
            activeSessionId={null}
            onSelectSession={vi.fn()}
            onArchive={vi.fn()}
          />
        </I18nProvider>,
      );

      expect(screen.getByText(expectedLabel)).toBeInTheDocument();
      expect(screen.queryByText(enChat.list.typing)).not.toBeInTheDocument();

      const spinners = container.querySelectorAll(".animate-spin");
      if (status === "running") {
        expect(spinners).toHaveLength(1);
      } else {
        expect(spinners).toHaveLength(0);
      }
    });
  }

  it("shows 'Queued 18m' in the list for a queued task created 18 minutes ago", () => {
    vi.useFakeTimers();
    const now = new Date("2026-10-21T09:18:00Z");
    vi.setSystemTime(now);
    mockPresenceByAgent.set("agent-1", { availability: "online" });
    mockPendingTasks.tasks = [
      {
        task_id: "t-queued-18m",
        status: "queued",
        chat_session_id: "s1",
        created_at: "2026-10-21T09:00:00Z",
      },
    ];

    renderList(null, { renderedSessions: [sessions[0]!] });

    expect(screen.getByText("Queued 18m")).toBeInTheDocument();
    vi.useRealTimers();
  });
});
