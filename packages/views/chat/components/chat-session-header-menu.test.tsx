import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Agent, ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import enCommon from "../../locales/en/common.json";

vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ agentDetail: (id: string) => `/agents/${id}` }),
}));
vi.mock("@multica/core/chat/mutations", () => ({
  useUpdateChatSession: () => ({ mutate: vi.fn() }),
  useDeleteChatSession: () => ({ mutate: vi.fn() }),
  useSetChatSessionArchived: () => ({ mutate: vi.fn() }),
}));
vi.mock("@multica/core/chat", () => ({
  useChatStore: (selector: (state: { setActiveSession: () => void }) => unknown) =>
    selector({ setActiveSession: vi.fn() }),
}));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => null }));
vi.mock("../../navigation", () => ({
  AppLink: ({ children, href: _href, ...props }: { children: React.ReactNode; href?: string }) => (
    <a {...props}>{children}</a>
  ),
}));

import { ChatSessionHeader } from "./chat-session-header";

const active: ChatSession = {
  id: "s1",
  workspace_id: "ws-1",
  agent_id: "agent-1",
  creator_id: "u1",
  title: "Dashboard rollout",
  status: "active",
  has_unread: false,
  unread_count: 0,
  last_message: null,
  pinned: false,
  created_at: new Date(0).toISOString(),
  updated_at: new Date(0).toISOString(),
};
const archived = { ...active, status: "archived" } as ChatSession;
const agent = { id: "agent-1", name: "Lambda", description: "" } as Agent;

function openMenu(props: Partial<React.ComponentProps<typeof ChatSessionHeader>> = {}) {
  const handlers = {
    onOpenStatistics: vi.fn(),
    onArchive: vi.fn(),
    onUnarchive: vi.fn(),
    onTogglePin: vi.fn(),
  };
  render(
    <I18nProvider locale="en" resources={{ en: { chat: enChat, common: enCommon } }}>
      <ChatSessionHeader session={active} agent={agent} {...handlers} {...props} />
    </I18nProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: enCommon.more_actions }));
  return handlers;
}

const labels = () => screen.getAllByRole("menuitem").map((item) => item.textContent?.trim());

describe("ChatSessionHeader more menu", () => {
  it("lists Statistics, Pin, Rename, profile and Archive in one order for an active DM", () => {
    openMenu();
    expect(labels()).toEqual([
      enChat.header.statistics,
      enChat.header.pin,
      enChat.header.rename,
      enChat.header.view_profile,
      enChat.header.archive,
    ]);
    expect(screen.getByTestId("chat-more-menu-statistics")).toBeInTheDocument();
    expect(screen.getByTestId("chat-more-menu-pin")).toBeInTheDocument();
    expect(screen.getByTestId("chat-more-menu-rename")).toBeInTheDocument();
    expect(screen.getByTestId("chat-more-menu-view-profile")).toBeInTheDocument();
    expect(screen.getByTestId("chat-more-menu-archive")).toBeInTheDocument();
  });

  it("opens the statistics dialog from the Statistics item", () => {
    const { onOpenStatistics } = openMenu();
    fireEvent.click(screen.getByTestId("chat-more-menu-statistics"));
    expect(onOpenStatistics).toHaveBeenCalledTimes(1);
  });

  it("pins an active DM and unpins a pinned one", () => {
    const first = openMenu();
    fireEvent.click(screen.getByTestId("chat-more-menu-pin"));
    expect(first.onTogglePin).toHaveBeenCalledWith(active);
  });

  it("offers Unpin for a pinned DM", () => {
    openMenu({ session: { ...active, pinned: true } as ChatSession });
    expect(screen.getByTestId("chat-more-menu-pin")).toHaveTextContent(enChat.header.unpin);
  });

  it("hands Archive to the owner, which awaits the server before navigating", () => {
    const { onArchive } = openMenu();
    fireEvent.click(screen.getByTestId("chat-more-menu-archive"));
    expect(onArchive).toHaveBeenCalledWith(active);
  });

  it("hides Pin for an archived DM and offers Unarchive and Delete", () => {
    const { onUnarchive } = openMenu({ session: archived });
    expect(labels()).toEqual([
      enChat.header.statistics,
      enChat.header.rename,
      enChat.header.view_profile,
      enChat.header.unarchive,
      enChat.header.delete,
    ]);
    expect(screen.queryByTestId("chat-more-menu-pin")).not.toBeInTheDocument();
    fireEvent.click(screen.getByTestId("chat-more-menu-archive"));
    expect(onUnarchive).toHaveBeenCalledWith(archived);
  });

  it("omits Statistics and Pin when the owner does not wire them", () => {
    openMenu({ onOpenStatistics: undefined, onTogglePin: undefined });
    expect(labels()).toEqual([
      enChat.header.rename,
      enChat.header.view_profile,
      enChat.header.archive,
    ]);
  });
});
