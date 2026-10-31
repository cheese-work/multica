import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Agent, ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import enIssues from "../../locales/en/issues.json";

const archiveAsync = vi.hoisted(() => vi.fn());
const pinMutate = vi.hoisted(() => vi.fn());
const guard = vi.hoisted(() => ({
  beginSessionAction: vi.fn(() => true),
  endSessionAction: vi.fn(),
}));

vi.mock("sonner", () => ({ toast: Object.assign(vi.fn(), { error: vi.fn() }) }));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => null }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/agents", () => ({ useWorkspacePresenceMap: () => ({ byAgent: new Map() }) }));
vi.mock("@multica/core/api", () => ({ api: { cancelTaskById: vi.fn() } }));
vi.mock("@multica/core/chat", () => ({
  useChatStore: Object.assign(
    (selector: (s: { setActiveSession: () => void }) => unknown) => selector({ setActiveSession: vi.fn() }),
    { getState: () => guard },
  ),
}));
vi.mock("@multica/core/chat/mutations", () => ({
  useDeleteChatSession: () => ({ mutate: vi.fn(), isPending: false }),
  useSetChatSessionPinned: () => ({ mutate: pinMutate, mutateAsync: vi.fn(), isPending: false }),
  useSetChatSessionArchived: () => ({ mutate: vi.fn(), mutateAsync: archiveAsync, isPending: false }),
}));
vi.mock("@tanstack/react-query", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-query")>();
  return {
    ...actual,
    useQuery: () => ({ data: { tasks: [] } }),
    useQueryClient: () => ({ setQueryData: vi.fn(), invalidateQueries: vi.fn() }),
  };
});

import { ChatThreadList } from "./chat-thread-list";

function makeSession(over: Partial<ChatSession> & Pick<ChatSession, "id">): ChatSession {
  return {
    workspace_id: "ws-1",
    agent_id: "agent-1",
    creator_id: "user-1",
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

// A legacy cache row can still carry a pin on an archived DM; the list must
// ignore it. The pinned archived row is the OLDER one.
const sessions = [
  makeSession({ id: "live", created_at: "2026-07-08T05:00:00Z" }),
  makeSession({ id: "old-pinned", status: "archived", pinned: true, created_at: "2026-07-08T01:00:00Z" }),
  makeSession({ id: "new-plain", status: "archived", pinned: false, created_at: "2026-07-08T03:00:00Z" }),
];

function openArchivedView() {
  render(
    <I18nProvider locale="en" resources={{ en: { chat: enChat, issues: enIssues } }}>
      <ChatThreadList
        sessions={sessions}
        agents={[agent]}
        activeSessionId={null}
        onSelectSession={vi.fn()}
        onArchive={vi.fn()}
      />
    </I18nProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: new RegExp(enChat.list.archived_title) }));
}

beforeEach(() => {
  vi.clearAllMocks();
  guard.beginSessionAction.mockReturnValue(true);
});

describe("ChatThreadList archived view", () => {
  it("sorts by last activity only, ignoring a stale pin", () => {
    openArchivedView();
    const titles = screen.getAllByText(/^Chat /).map((node) => node.textContent);
    expect(titles).toEqual(["Chat new-plain", "Chat old-pinned"]);
  });

  it("shows no pin icon and offers no Pin item on an archived row", async () => {
    openArchivedView();
    expect(screen.queryByLabelText(enChat.list.pinned)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: enChat.list.pin })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: enChat.list.unpin })).not.toBeInTheDocument();
    const rows = screen.getAllByRole("button", { name: enChat.list.row_actions_aria });
    fireEvent.click(rows[0]!);
    const menu = await screen.findAllByRole("menuitem");
    expect(menu.map((item) => item.textContent?.trim())).not.toContain(enChat.list.pin);
    expect(menu.map((item) => item.textContent?.trim())).not.toContain(enChat.list.unpin);
  });

  it("unarchives through the shared guarded action", async () => {
    archiveAsync.mockResolvedValue(undefined);
    openArchivedView();

    fireEvent.click(screen.getAllByRole("button", { name: enChat.list.unarchive })[0]!);

    await waitFor(() =>
      expect(archiveAsync).toHaveBeenCalledWith({ sessionId: "new-plain", archived: false }),
    );
    expect(guard.beginSessionAction).toHaveBeenCalledWith("new-plain", "unarchive");
    expect(pinMutate).not.toHaveBeenCalled();
  });

  it("keeps the pin icon and Pin item on an active row", () => {
    const pinned = [makeSession({ id: "live", pinned: true })];
    render(
      <I18nProvider locale="en" resources={{ en: { chat: enChat, issues: enIssues } }}>
        <ChatThreadList
          sessions={pinned}
          agents={[agent]}
          activeSessionId={null}
          onSelectSession={vi.fn()}
          onArchive={vi.fn()}
        />
      </I18nProvider>,
    );
    const row = screen.getByText("Chat live").closest("[tabindex]") as HTMLElement;
    expect(within(row).getByLabelText(enChat.list.pinned)).toBeInTheDocument();
    expect(within(row).getByRole("button", { name: enChat.list.unpin })).toBeInTheDocument();
  });
});
