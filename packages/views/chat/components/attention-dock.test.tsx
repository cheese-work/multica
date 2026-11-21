import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Agent, ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import { NavigationProvider } from "../../navigation";

const layout = vi.hoisted(() => ({ below: false }));
vi.mock("@multica/ui/hooks/use-mobile", () => ({
  useIsMobile: () => false,
  useIsCompact: () => false,
  useIsBelowDock: () => layout.below,
}));
vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...await importOriginal<typeof import("@multica/core/paths")>(),
  useWorkspaceSlug: () => "test-ws",
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/api", () => ({ api: {} }));

import { AttentionDock, AttentionDrawer } from "./attention-dock";

const NOW = Date.now();
const hoursAgo = (h: number) => new Date(NOW - h * 3_600_000).toISOString();
const session = (overrides: Partial<ChatSession> & Pick<ChatSession, "id">): ChatSession =>
  ({
    workspace_id: "ws-1",
    agent_id: "agent-1",
    creator_id: "user-1",
    title: `DM ${overrides.id}`,
    status: "active",
    has_unread: false,
    created_at: hoursAgo(100),
    updated_at: hoursAgo(1),
    stale_level: "none",
    attention_source: null,
    attention_since: null,
    next_change_at: null,
    ...overrides,
  }) as ChatSession;
const red = (id: string) =>
  session({ id, stale_level: "red", attention_source: "ask", attention_since: hoursAgo(26) });
const agents = [{ id: "agent-1", name: "Alpha" } as unknown as Agent];

function renderBoth(sessions: ChatSession[]) {
  const client = new QueryClient();
  render(
    <QueryClientProvider client={client}>
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
        <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/test-ws/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (p) => p }}>
          <AttentionDrawer sessions={sessions} agents={agents} />
          <AttentionDock sessions={sessions} agents={agents} />
        </NavigationProvider>
      </I18nProvider>
    </QueryClientProvider>,
  );
}

describe("Needs you: docked panel and drawer (D0 lock, 1280px)", () => {
  beforeEach(() => {
    layout.below = false;
  });

  it("docks the panel beside the conversation at 1280px and wider, with no drawer trigger", () => {
    renderBoth([red("r1"), red("r2")]);
    const dock = screen.getByRole("complementary", { name: "Needs you" });
    expect(within(dock).getAllByTestId("attention-row")).toHaveLength(2);
    expect(screen.queryByRole("button", { name: /Needs you/ })).not.toBeInTheDocument();
  });

  it("below 1280px the panel folds into a drawer that opens from a labelled button", () => {
    layout.below = true;
    renderBoth([red("r1"), red("r2")]);
    expect(screen.queryByRole("complementary")).not.toBeInTheDocument();
    expect(screen.queryByTestId("attention-row")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Needs you 2" }));
    const drawer = screen.getByRole("dialog", { name: "Needs you" });
    expect(within(drawer).getAllByTestId("attention-row")).toHaveLength(2);
  });

  it("shows neither the dock nor the drawer button when nothing needs you", () => {
    renderBoth([session({ id: "quiet" })]);
    expect(screen.queryByRole("complementary")).not.toBeInTheDocument();
    layout.below = true;
    renderBoth([session({ id: "quiet" })]);
    expect(screen.queryByRole("button", { name: /Needs you/ })).not.toBeInTheDocument();
  });
});
