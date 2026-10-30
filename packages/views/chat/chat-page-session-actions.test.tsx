// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ChatSession } from "@multica/core/types";
import { useChatStore } from "@multica/core/chat";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../locales/en/common.json";
import enChat from "../locales/en/chat.json";
import { NavigationProvider, type NavigationAdapter } from "../navigation";

const TEST_RESOURCES = { en: { common: enCommon, chat: enChat } };

const archiveAsync = vi.hoisted(() => vi.fn());
const pinAsync = vi.hoisted(() => vi.fn());
const toastError = vi.hoisted(() => vi.fn());
const toastShow = vi.hoisted(() => Object.assign(vi.fn(), { error: toastError }));
const layout = vi.hoisted(() => ({ width: 1440 }));
const controller = vi.hoisted(() => ({
  session: null as unknown,
  sessions: [] as unknown[],
  handleSend: vi.fn(),
  advanceSelectionAfterArchive: vi.fn(),
  setActiveSession: vi.fn(),
}));
const lastInputProps = vi.hoisted(() => ({ current: {} as Record<string, unknown> }));

vi.mock("sonner", () => ({ toast: toastShow }));
vi.mock("../auth/use-logout", () => ({ useLogout: vi.fn() }));
vi.mock("./components/chat-message-list", () => ({
  ChatMessageList: () => <div>chat-message-list</div>,
  ChatMessageSkeleton: () => <div>chat-message-skeleton</div>,
}));
vi.mock("./components/chat-input", () => ({
  ChatInput: (props: Record<string, unknown>) => {
    lastInputProps.current = props;
    const call = (name: string) => () => void (props[name] as (() => unknown) | undefined)?.();
    return (
      <div>
        <button type="button" onClick={call("onStatistic")}>cmd-statistic</button>
        <button type="button" onClick={call("onArchive")}>cmd-archive</button>
        <button type="button" onClick={call("onPin")}>cmd-pin</button>
        <button type="button" onClick={call("onUnarchive")}>cmd-unarchive</button>
        <button
          type="button"
          onClick={() =>
            void (props.onSend as (...a: unknown[]) => unknown)("hello", undefined, vi.fn(), [])
          }
        >
          send-message
        </button>
      </div>
    );
  },
}));
vi.mock("./components/chat-thread-list", () => ({
  ChatThreadList: ({ onArchive, sessions }: { onArchive: (s: unknown) => void; sessions: unknown[] }) => (
    <button type="button" onClick={() => onArchive(sessions[0])}>
      row-archive
    </button>
  ),
}));
vi.mock("./components/chat-session-header", () => ({
  ChatSessionHeader: (props: Record<string, (...a: unknown[]) => unknown>) => (
    <div>
      <button type="button" onClick={() => props.onOpenStatistics?.()}>menu-statistics</button>
      <button type="button" onClick={() => props.onArchive?.(controller.session)}>menu-archive</button>
      <button type="button" onClick={() => props.onUnarchive?.(controller.session)}>menu-unarchive</button>
      <button type="button" onClick={() => props.onTogglePin?.(controller.session)}>menu-pin</button>
    </div>
  ),
}));
vi.mock("./components/chat-session-stats-dialog", () => ({
  ChatSessionStatsDialog: ({ open, sessionId }: { open: boolean; sessionId: string | null }) =>
    open ? <div data-testid="stats-open">{sessionId}</div> : null,
}));
vi.mock("./components/chat-empty-state", () => ({ EmptyState: () => <div>empty</div> }));
vi.mock("./components/new-chat-button", () => ({ NewChatButton: () => null }));
vi.mock("./components/offline-banner", () => ({ OfflineBanner: () => null }));
vi.mock("./components/no-agent-banner", () => ({ NoAgentBanner: () => null }));
vi.mock("./components/archived-agent-banner", () => ({ ArchivedAgentBanner: () => null }));
vi.mock("react-resizable-panels", () => ({
  useDefaultLayout: () => ({ defaultLayout: undefined, onLayoutChanged: vi.fn() }),
}));
vi.mock("@multica/ui/components/ui/resizable", () => ({
  ResizablePanelGroup: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
  ResizablePanel: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
  ResizableHandle: () => null,
}));
vi.mock("@multica/ui/hooks/use-mobile", () => ({
  useIsMobile: () => layout.width < 768,
  useIsCompact: () => layout.width < 1024,
}));
vi.mock("@multica/core/paths", () => ({
  useRequiredWorkspaceSlug: () => "acme",
  useCurrentWorkspace: () => ({ settings: {} }),
  useWorkspacePaths: () => ({ chat: () => "/acme/chat" }),
}));
vi.mock("@multica/core/hooks", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/hooks")>()),
  useWorkspaceId: () => "ws-1",
}));
vi.mock("@multica/core/platform", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/platform")>()),
  getCurrentSlug: () => "acme",
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
vi.mock("@multica/core/chat/mutations", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/chat/mutations")>()),
  useSetChatSessionArchived: () => ({ mutateAsync: archiveAsync, mutate: vi.fn() }),
  useSetChatSessionPinned: () => ({ mutateAsync: pinAsync, mutate: vi.fn() }),
}));
vi.mock("./components/use-chat-controller", async () => {
  const { useChatStore: store } = await import("@multica/core/chat");
  return {
    useChatController: () => {
      const activeSessionId = store((s) => s.activeSessionId);
      const session = controller.session as ChatSession | null;
      return {
        wsId: "ws-1",
        user: { id: "user-1" },
        agents: [],
        availableAgents: [],
        agentsSettled: true,
        sessions: controller.sessions,
        interactiveLoginRequired: false,
        activeSessionId,
        selectedAgentId: null,
        currentSession: session,
        isSessionArchived: session?.status === "archived",
        isAgentArchived: false,
        isAgentRuntimeBound: true,
        activeAgent: null,
        noAgent: false,
        availability: "online",
        messages: [],
        pendingTask: null,
        pendingTaskId: null,
        showSkeleton: false,
        hasMessages: false,
        firstItemIndex: 0,
        hasOlderMessages: false,
        isFetchingOlderMessages: false,
        fetchOlderMessages: vi.fn(),
        restoreDraftRequest: null,
        handleRestoreDraftApplied: vi.fn(),
        focusInputRequest: 0,
        handleSend: controller.handleSend,
        handleStop: vi.fn(),
        handleNewChat: vi.fn(),
        handleStartNewChat: vi.fn(),
        handleSelectSession: (s: { id: string }) => store.getState().setActiveSession(s.id),
        advanceSelectionAfterArchive: controller.advanceSelectionAfterArchive,
        setActiveSession: controller.setActiveSession,
        setSelectedAgentId: vi.fn(),
      };
    },
  };
});

import { ChatPage } from "./chat-page";

const active = {
  id: "s1",
  agent_id: "a1",
  status: "active",
  pinned: false,
  title: "Dashboard rollout",
  continued_into_session_id: null,
} as unknown as ChatSession;
const archived = { ...active, status: "archived" } as ChatSession;

function renderPage(search = "?session=s1") {
  const navigation = {
    push: vi.fn(),
    replace: vi.fn(),
    back: vi.fn(),
    canGoBack: () => true,
    pathname: "/acme/chat",
    searchParams: new URLSearchParams(search),
    hash: "",
    getShareableUrl: (path: string) => path,
  } as unknown as NavigationAdapter;
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <NavigationProvider value={navigation}>
          <ChatPage />
        </NavigationProvider>
      </I18nProvider>
    </QueryClientProvider>,
  );
  return navigation;
}

beforeEach(() => {
  vi.clearAllMocks();
  layout.width = 1440;
  controller.session = active;
  controller.sessions = [active];
  archiveAsync.mockReset();
  pinAsync.mockReset();
  useChatStore.setState({ activeSessionId: "s1", sessionActionPending: {} });
});

describe("ChatPage statistics, archive and pin", () => {
  it("opens the statistics dialog from the header menu", () => {
    renderPage();
    expect(screen.queryByTestId("stats-open")).not.toBeInTheDocument();
    fireEvent.click(screen.getByText("menu-statistics"));
    expect(screen.getByTestId("stats-open")).toHaveTextContent("s1");
  });

  it("opens the same dialog for /statistic and sends no message", () => {
    renderPage();
    fireEvent.click(screen.getByText("cmd-statistic"));
    expect(screen.getByTestId("stats-open")).toHaveTextContent("s1");
    expect(controller.handleSend).not.toHaveBeenCalled();
  });

  it("advances the desktop selection only after the server accepted the archive", async () => {
    const order: string[] = [];
    archiveAsync.mockImplementation(async () => void order.push("server"));
    controller.advanceSelectionAfterArchive.mockImplementation(() => void order.push("advance"));
    renderPage();

    fireEvent.click(screen.getByText("menu-archive"));

    await waitFor(() => expect(controller.advanceSelectionAfterArchive).toHaveBeenCalledWith(active));
    expect(order).toEqual(["server", "advance"]);
    expect(archiveAsync).toHaveBeenCalledWith({ sessionId: "s1", archived: true });
  });

  it("goes back through history after a compact archive, not to a cleared selection", async () => {
    layout.width = 800;
    archiveAsync.mockResolvedValue(undefined);
    const navigation = renderPage();

    fireEvent.click(screen.getByText("menu-archive"));

    await waitFor(() => expect(navigation.back).toHaveBeenCalledTimes(1));
    expect(controller.setActiveSession).not.toHaveBeenCalled();
    expect(controller.advanceSelectionAfterArchive).not.toHaveBeenCalled();
  });

  it("keeps the session open and shows the server error when the archive fails", async () => {
    archiveAsync.mockRejectedValue(new Error("This DM is in a decision lane"));
    const navigation = renderPage();

    fireEvent.click(screen.getByText("menu-archive"));

    await waitFor(() => expect(toastError).toHaveBeenCalledWith("This DM is in a decision lane"));
    expect(controller.advanceSelectionAfterArchive).not.toHaveBeenCalled();
    expect(navigation.back).not.toHaveBeenCalled();
    expect(useChatStore.getState().activeSessionId).toBe("s1");
  });

  it("archives from /archive through the same path and sends no message", async () => {
    archiveAsync.mockResolvedValue(undefined);
    renderPage();

    fireEvent.click(screen.getByText("cmd-archive"));

    await waitFor(() => expect(controller.advanceSelectionAfterArchive).toHaveBeenCalledWith(active));
    expect(controller.handleSend).not.toHaveBeenCalled();
  });

  it("archives a thread-list row through the same path", async () => {
    archiveAsync.mockResolvedValue(undefined);
    renderPage();
    fireEvent.click(screen.getByText("row-archive"));
    await waitFor(() => expect(archiveAsync).toHaveBeenCalledWith({ sessionId: "s1", archived: true }));
  });

  it("pins from the menu and from /pin", async () => {
    pinAsync.mockResolvedValue(undefined);
    renderPage();
    fireEvent.click(screen.getByText("menu-pin"));
    await waitFor(() => expect(pinAsync).toHaveBeenCalledWith({ sessionId: "s1", pinned: true }));
    fireEvent.click(screen.getByText("cmd-pin"));
    await waitFor(() => expect(pinAsync).toHaveBeenCalledTimes(2));
    expect(controller.handleSend).not.toHaveBeenCalled();
  });
});

describe("ChatPage composer on an active and an archived DM", () => {
  it("wires statistic, archive and pin, and no unarchive, for an active DM", () => {
    renderPage();
    const props = lastInputProps.current;
    expect(props.onStatistic).toBeTypeOf("function");
    expect(props.onArchive).toBeTypeOf("function");
    expect(props.onPin).toBeTypeOf("function");
    expect(props.onUnarchive).toBeUndefined();
    expect(props.sessionArchived).toBeFalsy();
  });

  it("keeps the composer available on an archived DM with only unarchive wired", () => {
    controller.session = archived;
    controller.sessions = [archived];
    renderPage();
    const props = lastInputProps.current;
    expect(props.disabled).toBe(false);
    expect(props.sessionArchived).toBe(true);
    expect(props.onUnarchive).toBeTypeOf("function");
    expect(props.onArchive).toBeUndefined();
    expect(props.onPin).toBeUndefined();
    expect(props.onStatistic).toBeUndefined();
    expect(props.onRename).toBeUndefined();
    expect(props.onFork).toBeUndefined();
  });

  it("unarchives from /unarchive without sending a message", async () => {
    controller.session = archived;
    controller.sessions = [archived];
    archiveAsync.mockResolvedValue(undefined);
    renderPage();

    fireEvent.click(screen.getByText("cmd-unarchive"));

    await waitFor(() => expect(archiveAsync).toHaveBeenCalledWith({ sessionId: "s1", archived: false }));
    expect(controller.handleSend).not.toHaveBeenCalled();
  });

  it("holds a send from an archived DM until the user confirms the unarchive", async () => {
    controller.session = archived;
    controller.sessions = [archived];
    archiveAsync.mockResolvedValue(undefined);
    controller.handleSend.mockResolvedValue(true);
    renderPage();

    fireEvent.click(screen.getByText("send-message"));
    expect(controller.handleSend).not.toHaveBeenCalled();
    expect(archiveAsync).not.toHaveBeenCalled();

    await act(async () => {
      fireEvent.click(await screen.findByRole("button", { name: "Unarchive and send" }));
    });

    await waitFor(() => expect(controller.handleSend).toHaveBeenCalledTimes(1));
    expect(archiveAsync).toHaveBeenCalledWith({ sessionId: "s1", archived: false });
    expect(controller.handleSend).toHaveBeenCalledWith("hello", undefined, expect.any(Function), []);
  });

  it("sends from an active DM without asking", () => {
    controller.handleSend.mockResolvedValue(true);
    renderPage();
    fireEvent.click(screen.getByText("send-message"));
    expect(controller.handleSend).toHaveBeenCalledTimes(1);
    expect(screen.queryByTestId("chat-archived-send-dialog")).not.toBeInTheDocument();
  });
});
