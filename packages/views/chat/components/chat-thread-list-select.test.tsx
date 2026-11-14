import { afterEach, describe, expect, it, vi, beforeEach } from "vitest";
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
const batchAsync = vi.fn().mockResolvedValue({ failed: [] });
const mobile = { value: false };
const pinMutate = vi.fn();
const markUnreadAsync = vi.fn().mockResolvedValue(undefined);
const updateMutate = vi.fn();
const needsYou = { rows: [] as Record<string, unknown>[] };
const navigate = vi.fn();

vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...await importOriginal<typeof import("@multica/core/paths")>(),
  useWorkspaceSlug: () => "test-ws",
}));

vi.mock("@multica/ui/hooks/use-mobile", () => ({ useIsMobile: () => mobile.value }));

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
  useBatchChatSessions: () => ({ mutateAsync: batchAsync, isPending: false }),
  useMarkChatSessionRead: () => ({ mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false }),
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
const rowOf = (title: string) => screen.getByText(title).closest("[tabindex]") as HTMLElement;

beforeEach(() => {
  useChatStore.setState({ listView: "main" });
  mobile.value = false;
  batchAsync.mockReset().mockResolvedValue({ failed: [] });
});
afterEach(cleanup);

const mixed: ChatSession[] = [
  makeSession({ id: "s1", updated_at: "2026-07-08T03:00:00Z", has_unread: true, unread_count: 1 }),
  makeSession({ id: "s2", updated_at: "2026-07-08T02:00:00Z", has_unread: true, unread_count: 2 }),
  makeSession({ id: "s3", updated_at: "2026-07-08T01:00:00Z" }),
];

function enter(title = "Chat s1") {
  fireEvent.keyDown(rowOf(title), { key: "x" });
}

describe("ChatThreadList select mode", () => {
  it("X enters select mode with that row checked and shows the bulk bar", () => {
    renderList(null, { renderedSessions: mixed });
    enter();
    expect(screen.getByRole("checkbox", { name: "Select Chat s1" })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByRole("checkbox", { name: "Select Chat s2" })).toHaveAttribute("aria-checked", "false");
    expect(screen.getByText("1 selected")).toBeInTheDocument();
    expect(screen.getByRole("toolbar", { name: "Actions for 1 selected chat" })).toBeInTheDocument();
  });

  it("a row click toggles instead of opening the chat", () => {
    const { onSelectSession } = renderList(null, { renderedSessions: mixed });
    enter();
    fireEvent.click(rowOf("Chat s2"));
    expect(onSelectSession).not.toHaveBeenCalled();
    expect(screen.getByRole("checkbox", { name: "Select Chat s2" })).toHaveAttribute("aria-checked", "true");
  });

  it("the day header checkbox is unchecked / mixed / checked for none / some / all rows and toggles the group", () => {
    renderList(null, { renderedSessions: mixed });
    enter();
    const header = () => screen.getAllByRole("checkbox").find((c) => /^Select group/.test(c.getAttribute("aria-label") ?? ""))!;
    expect(header()).toHaveAttribute("aria-checked", "mixed");
    fireEvent.click(header()); // some -> all
    expect(header()).toHaveAttribute("aria-checked", "true");
    expect(screen.getByText("3 selected")).toBeInTheDocument();
    fireEvent.click(header()); // all -> none
    expect(header()).toHaveAttribute("aria-checked", "false");
    expect(screen.getByText("0 selected")).toBeInTheDocument();
  });

  it("checkboxes have a hit area of at least 44x44", () => {
    renderList(null, { renderedSessions: mixed });
    enter();
    for (const cb of screen.getAllByRole("checkbox")) {
      expect(cb.className).toMatch(/\bsize-11\b/);
      expect(cb.className).toMatch(/focus-visible:ring-2/);
    }
  });

  it("bulk bar: primary by rule, runs the batch, then leaves select mode; no delete", async () => {
    renderList(null, { renderedSessions: mixed });
    enter();
    fireEvent.click(screen.getByRole("button", { name: "Select all" }));
    expect(screen.getByText("3 selected")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /delete/i })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Mark seen" }));
    await waitFor(() => expect(batchAsync).toHaveBeenCalledWith({ action: "seen", sessionIds: ["s1", "s2"] }));
    await waitFor(() => expect(screen.queryByRole("toolbar", { name: /^Actions for/ })).toBeNull());
  });

  it("phone: the bar is icon-only", () => {
    mobile.value = true;
    renderList(null, { renderedSessions: mixed });
    enter();
    expect(screen.getByRole("button", { name: "Mark seen" }).textContent?.trim()).toBe("");
  });

  it("Cancel and Escape leave select mode and the row ⋯ menu returns", () => {
    renderList(null, { renderedSessions: mixed });
    enter();
    fireEvent.click(screen.getByRole("button", { name: "Cancel selection" }));
    expect(screen.queryByRole("checkbox")).toBeNull();
    enter();
    fireEvent.keyDown(rowOf("Chat s2"), { key: "Escape" });
    expect(screen.queryByRole("checkbox")).toBeNull();
  });
});
