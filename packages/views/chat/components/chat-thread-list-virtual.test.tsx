import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Agent, ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import enIssues from "../../locales/en/issues.json";
import { NavigationProvider } from "../../navigation";
import { useChatStore } from "@multica/core/chat";

// The DM list mounts a bounded window of rows, not one per session
// (CHE-5773). jsdom has no layout, so react-virtuoso is replaced by a probe
// that records the props the list hands it.
const virtuoso = vi.hoisted(() => ({ props: null as null | Record<string, unknown> }));
vi.mock("react-virtuoso", () => ({
  GroupedVirtuoso: (props: Record<string, unknown>) => {
    virtuoso.props = props;
    return <div data-testid="grouped-virtuoso" />;
  },
}));

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
vi.mock("@multica/core/chat/mutations", () => {
  const idle = () => ({ mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false });
  return {
    useDeleteChatSession: idle,
    useSetChatSessionPinned: idle,
    useMarkChatSessionUnread: idle,
    useMarkChatSessionRead: idle,
    useUpdateChatSession: idle,
    useSetChatSessionArchived: idle,
    useBatchChatSessions: idle,
  };
});
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

const agent = { id: "agent-1", name: "Alpha" } as unknown as Agent;
const sessions: ChatSession[] = Array.from({ length: 300 }, (_, i) => ({
  id: `s${i}`,
  workspace_id: "ws-1",
  agent_id: "agent-1",
  creator_id: "user-1",
  title: `Chat ${i}`,
  status: "active",
  has_unread: false,
  unread_count: 0,
  last_message: null,
  pinned: false,
  created_at: new Date(0).toISOString(),
  updated_at: new Date(Date.now() - i * 60_000).toISOString(),
}));

function renderList(scrollable: boolean) {
  const list = (
    <I18nProvider locale="en" resources={{ en: { chat: enChat, issues: enIssues } }}>
      <NavigationProvider
        value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/test-ws/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (p) => p }}
      >
        <ChatThreadList
          sessions={sessions}
          agents={[agent]}
          activeSessionId={null}
          onSelectSession={vi.fn()}
          onArchive={vi.fn()}
        />
      </NavigationProvider>
    </I18nProvider>
  );
  // jsdom applies no stylesheet, so the scroll container is declared inline.
  return render(scrollable ? <div style={{ overflowY: "auto" }}>{list}</div> : list);
}

beforeEach(() => {
  virtuoso.props = null;
  useChatStore.setState({ listView: "main" });
});
afterEach(cleanup);

describe("ChatThreadList virtualization", () => {
  it("mounts a bounded number of rows for 300 sessions before the scroll parent is known", () => {
    renderList(false);
    expect(screen.getAllByTestId("chat-thread-row").length).toBeLessThanOrEqual(40);
  });

  it("hands every session to GroupedVirtuoso once the scroll parent exists", () => {
    renderList(true);
    expect(screen.getByTestId("grouped-virtuoso")).toBeTruthy();
    expect(screen.queryAllByTestId("chat-thread-row")).toHaveLength(0);
    const counts = virtuoso.props!.groupCounts as number[];
    expect(counts.reduce((a, b) => a + b, 0)).toBe(300);
    expect(virtuoso.props!.customScrollParent).toBeInstanceOf(HTMLElement);
  });
});
