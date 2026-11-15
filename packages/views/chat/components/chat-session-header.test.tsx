import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Agent, ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import enCommon from "../../locales/en/common.json";

const updateMutate = vi.hoisted(() => vi.fn());
const pushMock = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ agentDetail: (id: string) => `/agents/${id}` }),
}));

vi.mock("@multica/core/chat/mutations", () => ({
  useUpdateChatSession: () => ({ mutate: updateMutate }),
  useDeleteChatSession: () => ({ mutate: vi.fn() }),
  useSetChatSessionArchived: () => ({ mutate: vi.fn() }),
}));

vi.mock("@multica/core/chat", () => ({
  useChatStore: (selector: (state: { setActiveSession: () => void }) => unknown) =>
    selector({ setActiveSession: vi.fn() }),
}));

vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: () => null,
}));

vi.mock("../../navigation", () => ({
  AppLink: ({ children, href }: { children: React.ReactNode; href: string }) => (
    <a href={href}>{children}</a>
  ),
  useNavigation: () => ({ push: pushMock }),
}));

vi.mock("./open-decisions-badge", () => ({ OpenDecisionsBadge: () => null }));
vi.mock("./scheduled-wakeups-chip", () => ({ ScheduledWakeupsChip: () => null }));

import { ChatSessionHeader } from "./chat-session-header";

const TEST_RESOURCES = { en: { chat: enChat, common: enCommon } };
const RENAME_LABEL = enChat.header.rename;
const OUTSIDE_LABEL = "Outside control";

const session: ChatSession = {
  id: "session-1",
  workspace_id: "ws-1",
  agent_id: "agent-1",
  creator_id: "user-1",
  title: "Original title",
  status: "active",
  has_unread: false,
  unread_count: 0,
  last_message: null,
  pinned: false,
  created_at: new Date(0).toISOString(),
  updated_at: new Date(0).toISOString(),
};

describe("ChatSessionHeader breadcrumb", () => {
  it("leads with the agent as a link to its page, then the chat title", () => {
    const agent = { id: "agent-1", name: "Lambda" } as Agent;
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ChatSessionHeader session={session} agent={agent} />
      </I18nProvider>,
    );

    const header = within(screen.getByRole("banner"));
    const agentLink = header.getByRole("link", { name: "Lambda" });
    expect(agentLink).toHaveAttribute("href", "/agents/agent-1");
    const title = header.getByRole("button", { name: "Original title" });
    expect(agentLink).not.toContainElement(title);
    expect(
      agentLink.compareDocumentPosition(title) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("opens the agent's settings from the ⋯ menu", () => {
    const agent = { id: "agent-1", name: "Lambda" } as Agent;
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ChatSessionHeader session={session} agent={agent} />
      </I18nProvider>,
    );

    fireEvent.click(screen.getByRole("button", { name: "More actions" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Agent settings" }));
    expect(pushMock).toHaveBeenCalledWith("/agents/agent-1?view=general");
  });

  it("offers no agent settings when the chat's agent is unavailable", () => {
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ChatSessionHeader session={session} agent={null} />
      </I18nProvider>,
    );

    fireEvent.click(screen.getByRole("button", { name: "More actions" }));
    expect(screen.getByRole("menuitem", { name: "Rename chat" })).toBeInTheDocument();
    expect(
      screen.queryByRole("menuitem", { name: "Agent settings" }),
    ).not.toBeInTheDocument();
  });

  it("renders a long title in full: no fixed character cap, full text in the tooltip", async () => {
    const longTitle = "tôi muốn adopt ý tưởng này vào hệ thống chat của chúng ta để tiêu đề hiển thị đầy đủ hơn ".repeat(2).trim();
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ChatSessionHeader session={{ ...session, title: longTitle }} agent={null} />
      </I18nProvider>,
    );
    const button = screen.getByRole("button", { name: longTitle });
    expect(button.textContent).toBe(longTitle);
    expect(button.className).toContain("truncate");
    act(() => button.focus());
    expect((await screen.findAllByText(longTitle)).length).toBeGreaterThan(1);
  });

  it("shows only the title when the chat's agent is unavailable", () => {
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ChatSessionHeader session={session} agent={null} />
      </I18nProvider>,
    );

    const header = within(screen.getByRole("banner"));
    expect(header.queryByRole("link")).not.toBeInTheDocument();
    expect(header.getByRole("button", { name: "Original title" })).toBeInTheDocument();
  });
});

function startRename(): HTMLInputElement {
  render(
    <>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ChatSessionHeader session={session} agent={null} />
      </I18nProvider>
      <button type="button">{OUTSIDE_LABEL}</button>
    </>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Original title" }));
  return screen.getByRole("textbox", { name: RENAME_LABEL });
}

describe("ChatSessionHeader rename keyboard behavior", () => {
  beforeEach(() => {
    updateMutate.mockReset();
  });

  it("opens inline rename with the current title selected on a command request", () => {
    const view = render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ChatSessionHeader session={session} agent={null} renameRequest={0} />
      </I18nProvider>,
    );
    view.rerender(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ChatSessionHeader session={session} agent={null} renameRequest={1} />
      </I18nProvider>,
    );
    const input = screen.getByRole<HTMLInputElement>("textbox", { name: RENAME_LABEL });
    expect(input).toHaveFocus();
    expect(input).toHaveValue(session.title);
    expect(input.selectionStart).toBe(0);
    expect(input.selectionEnd).toBe(session.title?.length);
    expect(updateMutate).not.toHaveBeenCalled();
  });

  it.each([
    ["standard composition signal", { isComposing: true, keyCode: 13 }],
    ["Safari composition signal", { isComposing: false, keyCode: 229 }],
  ])("keeps editing when Enter carries the %s", (_name, eventInit) => {
    const input = startRename();
    fireEvent.change(input, { target: { value: "yanjiu" } });

    fireEvent.keyDown(input, { key: "Enter", ...eventInit });

    expect(updateMutate).not.toHaveBeenCalled();
    expect(input).toBeInTheDocument();
    expect(input).toHaveValue("yanjiu");
  });

  it("submits once on a normal Enter", () => {
    const input = startRename();
    fireEvent.change(input, { target: { value: "Renamed chat" } });

    fireEvent.keyDown(input, { key: "Enter", isComposing: false, keyCode: 13 });

    expect(updateMutate).toHaveBeenCalledTimes(1);
    expect(updateMutate).toHaveBeenCalledWith({
      sessionId: "session-1",
      title: "Renamed chat",
    });
    expect(screen.queryByRole("textbox", { name: RENAME_LABEL })).not.toBeInTheDocument();
  });

  it("defers blur submission until an active composition ends", () => {
    const input = startRename();
    const outside = screen.getByRole("button", { name: OUTSIDE_LABEL });
    fireEvent.change(input, { target: { value: "yanjiu" } });
    fireEvent.compositionStart(input);

    act(() => {
      outside.focus();
    });

    expect(updateMutate).not.toHaveBeenCalled();
    expect(input).toBeInTheDocument();
    expect(outside).toHaveFocus();

    fireEvent.change(input, { target: { value: "研究" } });
    fireEvent.compositionEnd(input);

    expect(updateMutate).toHaveBeenCalledTimes(1);
    expect(updateMutate).toHaveBeenCalledWith({
      sessionId: "session-1",
      title: "研究",
    });
    expect(screen.queryByRole("textbox", { name: RENAME_LABEL })).not.toBeInTheDocument();
  });

  it("closes without saving a partial value when compositionend does not arrive", async () => {
    const input = startRename();
    const outside = screen.getByRole("button", { name: OUTSIDE_LABEL });
    fireEvent.change(input, { target: { value: "yanjiu" } });
    fireEvent.compositionStart(input);

    act(() => {
      outside.focus();
    });

    await waitFor(() => {
      expect(screen.queryByRole("textbox", { name: RENAME_LABEL })).not.toBeInTheDocument();
    });
    expect(updateMutate).not.toHaveBeenCalled();
    expect(screen.getByText("Original title")).toBeInTheDocument();
  });

  it("still submits the current value when focus moves outside", () => {
    const input = startRename();
    const outside = screen.getByRole("button", { name: OUTSIDE_LABEL });
    fireEvent.change(input, { target: { value: "Blurred title" } });

    act(() => {
      outside.focus();
    });

    expect(updateMutate).toHaveBeenCalledTimes(1);
    expect(updateMutate).toHaveBeenCalledWith({
      sessionId: "session-1",
      title: "Blurred title",
    });
    expect(outside).toHaveFocus();
  });

  it.each([
    ["standard composition signal", { isComposing: true, keyCode: 27 }],
    ["Safari composition signal", { isComposing: false, keyCode: 229 }],
  ])("keeps editing when Escape carries the %s", (_name, eventInit) => {
    const input = startRename();
    fireEvent.change(input, { target: { value: "yanjiu" } });

    fireEvent.keyDown(input, { key: "Escape", ...eventInit });

    expect(updateMutate).not.toHaveBeenCalled();
    expect(input).toBeInTheDocument();
    expect(input).toHaveValue("yanjiu");
  });

  it("still cancels the edit on Escape", () => {
    const input = startRename();
    fireEvent.change(input, { target: { value: "Discard me" } });

    fireEvent.keyDown(input, { key: "Escape" });

    expect(updateMutate).not.toHaveBeenCalled();
    expect(screen.queryByRole("textbox", { name: RENAME_LABEL })).not.toBeInTheDocument();
    expect(screen.getByText("Original title")).toBeInTheDocument();
  });
});

// CHE-1808: the context meter shows the latest completed run's prompt size
// against the model's window, warns near the auto-compact token
// threshold, and the menu offers "Compact now".
describe("ChatSessionHeader context meter", () => {
  const withContext = (tokens?: number | null, window?: number | null) =>
    ({ ...session, context_tokens: tokens, context_window: window }) as unknown as ChatSession;

  function renderHeader(
    sessionValue: ChatSession,
    props: { autoCompactThresholdTokens?: number | null; lastTurnTokens?: number | null; onCompact?: () => void } = {},
  ) {
    const Header = ChatSessionHeader as unknown as React.ComponentType<Record<string, unknown>>;
    return render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <Header session={sessionValue} agent={null} {...props} />
      </I18nProvider>,
    );
  }

  it.each([
    ["no size", undefined, undefined],
    ["no window", 124_000, null],
    ["no tokens", null, 200_000],
    ["zero window", 124_000, 0],
  ])("is hidden when the run reported %s", (_name, tokens, window) => {
    renderHeader(withContext(tokens, window), { autoCompactThresholdTokens: 160_000 });

    expect(screen.queryByText(/ of \d/)).not.toBeInTheDocument();
  });

  it("shows the share and the window below the warning zone", () => {
    renderHeader(withContext(124_000, 200_000), { autoCompactThresholdTokens: 160_000 });

    expect(screen.getByText("62% of 200k")).toBeInTheDocument();
    expect(screen.queryByText("Will compact after the next reply")).not.toBeInTheDocument();
  });

  // CHE-4418: a 1M window with the 200k default is a 20% threshold. The old
  // ten-point band warned at 10% (100k), half the threshold.
  it("does not warn at 110k of a 1M window with a 200k threshold", () => {
    renderHeader(withContext(110_000, 1_000_000), { autoCompactThresholdTokens: 200_000 });

    expect(screen.getByText("11% of 1M")).toBeInTheDocument();
    expect(screen.queryByText(/Compacts at/)).not.toBeInTheDocument();
    expect(screen.queryByText("Will compact after the next reply")).not.toBeInTheDocument();
  });

  it("does not warn one token short of 90% of the threshold tokens", () => {
    renderHeader(withContext(179_999, 1_000_000), { autoCompactThresholdTokens: 200_000 });
    expect(screen.queryByText(/Compacts at/)).not.toBeInTheDocument();
  });

  it("shows the threshold and current size from 90% of the threshold tokens", () => {
    renderHeader(withContext(180_000, 1_000_000), { autoCompactThresholdTokens: 200_000 });
    expect(screen.getByText("Compacts at 200k (now 180k)")).toBeInTheDocument();
    expect(screen.queryByText("Will compact after the next reply")).not.toBeInTheDocument();
  });

  it("uses the exact token threshold, not one rebuilt from a percent", () => {
    // 1,048,576 window with the 200k default: 20% rounds up to 209,715 tokens.
    const { unmount } = renderHeader(withContext(179_999, 1_048_576), { autoCompactThresholdTokens: 200_000 });
    expect(screen.queryByText(/Compacts at/)).not.toBeInTheDocument();
    unmount();

    renderHeader(withContext(180_000, 1_048_576), { autoCompactThresholdTokens: 200_000 });
    expect(screen.getByText("Compacts at 200k (now 180k)")).toBeInTheDocument();
  });

  it("uses the same rule on a 200k window at 80%", () => {
    const { unmount } = renderHeader(withContext(143_999, 200_000), { autoCompactThresholdTokens: 160_000 });
    expect(screen.queryByText(/Compacts at/)).not.toBeInTheDocument();
    unmount();

    renderHeader(withContext(144_000, 200_000), { autoCompactThresholdTokens: 160_000 });
    expect(screen.getByText("Compacts at 160k (now 144k)")).toBeInTheDocument();
  });

  it("keeps the old text only when less than one last turn remains", () => {
    const { unmount } = renderHeader(withContext(150_000, 1_000_000), {
      autoCompactThresholdTokens: 200_000,
      lastTurnTokens: 40_000,
    });
    expect(screen.queryByText("Will compact after the next reply")).not.toBeInTheDocument();
    expect(screen.queryByText(/Compacts at/)).not.toBeInTheDocument();
    unmount();

    renderHeader(withContext(170_000, 1_000_000), {
      autoCompactThresholdTokens: 200_000,
      lastTurnTokens: 40_000,
    });
    expect(screen.getByText("Will compact after the next reply")).toBeInTheDocument();
  });

  it("never warns when auto-compact is off", () => {
    renderHeader(withContext(190_000, 200_000), { autoCompactThresholdTokens: null });

    expect(screen.getByText("95% of 200k")).toBeInTheDocument();
    expect(screen.queryByText("Will compact after the next reply")).not.toBeInTheDocument();
  });

  it("writes a 1M window as 1M", () => {
    renderHeader(withContext(120_000, 1_000_000), { autoCompactThresholdTokens: 160_000 });

    expect(screen.getByText("12% of 1M")).toBeInTheDocument();
  });

  it("sends /compact from the menu's Compact now item", async () => {
    const onCompact = vi.fn();
    renderHeader(withContext(124_000, 200_000), { autoCompactThresholdTokens: 160_000, onCompact });

    fireEvent.click(screen.getByRole("button", { name: "More actions" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Compact now" }));

    expect(onCompact).toHaveBeenCalledTimes(1);
  });
});

