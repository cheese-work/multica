// @vitest-environment jsdom

import { StrictMode, useState } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useSyncExternalStore } from "react";
import type { Agent } from "@multica/core/types";
import { useChatStore } from "@multica/core/chat";
import { setCurrentWorkspace } from "@multica/core/platform";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../locales/en/common.json";
import enChat from "../locales/en/chat.json";
import {
  NavigationProvider,
  type NavigationAdapter,
} from "../navigation";

const TEST_RESOURCES = { en: { common: enCommon, chat: enChat } };
const mockSignIn = vi.hoisted(() => vi.fn());

vi.mock("../auth/use-logout", () => ({ useLogout: () => mockSignIn }));

// These tests target the page-level URL wiring (`?agent=` / `?session=`), so
// the conversation internals are stubbed and the controller is replaced with
// a ref-driven fake the tests can steer. The thread-list stub stays
// interactive: selecting a thread is the user action that must supersede a
// pending `?agent=` intent.
vi.mock("./components/chat-message-list", async () => {
  const { AppLink } = await import("../navigation");
  return {
    // Only the divider link is real: AppLink pushes through the adapter, as
    // the "continued from" link in the transcript does.
    ChatMessageList: ({
      continuedIntoSessionId,
      jumpToMessageId,
      onJumpSettled,
    }: {
      continuedIntoSessionId?: string | null;
      jumpToMessageId?: string | null;
      onJumpSettled?: (outcome: "landed" | "missing") => void;
    }) => (
      <div>
        chat-message-list
        <span data-testid="jump-target">{jumpToMessageId ?? ""}</span>
        <button type="button" onClick={() => onJumpSettled?.("landed")}>settle-landed</button>
        <button type="button" onClick={() => onJumpSettled?.("missing")}>settle-missing</button>
        {continuedIntoSessionId && (
          <AppLink href={`/acme/chat?session=${continuedIntoSessionId}`}>continued-link</AppLink>
        )}
      </div>
    ),
    ChatMessageSkeleton: () => <div>chat-message-skeleton</div>,
  };
});
vi.mock("./components/chat-input", () => ({
  ChatInput: ({ disabled, compacting }: { disabled?: boolean; compacting?: boolean }) => (
    <div data-disabled={String(!!disabled)} data-compacting={String(!!compacting)}>
      chat-input
    </div>
  ),
}));
vi.mock("./components/chat-thread-list", () => ({
  ChatThreadList: ({
    onSelectSession,
    onOpenSearchHit,
  }: {
    onSelectSession: (s: { id: string; agent_id: string }) => void;
    onOpenSearchHit?: (hit: { session_id: string; message_id: string | null }) => void;
  }) => (
    <>
      <button
        type="button"
        onClick={() => onSelectSession({ id: "session-9", agent_id: "agent-9" })}
      >
        select-thread
      </button>
      <button
        type="button"
        onClick={() => onOpenSearchHit?.({ session_id: "session-9", message_id: "msg-4" })}
      >
        open-message-hit
      </button>
    </>
  ),
}));
vi.mock("./components/chat-session-header", () => ({
  ChatSessionHeader: () => <div>chat-session-header</div>,
}));
vi.mock("./components/chat-empty-state", () => ({
  EmptyState: () => <div>chat-empty-state</div>,
}));
vi.mock("./components/new-chat-button", () => ({
  NewChatButton: ({ onStart }: { onStart: (agent: null) => void }) => (
    <button type="button" onClick={() => onStart(null)}>
      new-chat
    </button>
  ),
}));
vi.mock("./components/offline-banner", () => ({
  OfflineBanner: () => null,
}));
vi.mock("./components/no-agent-banner", () => ({
  NoAgentBanner: () => null,
}));
vi.mock("./components/archived-agent-banner", () => ({
  ArchivedAgentBanner: () => null,
}));
vi.mock("react-resizable-panels", () => ({
  useDefaultLayout: () => ({ defaultLayout: undefined, onLayoutChanged: vi.fn() }),
}));
vi.mock("@multica/ui/components/ui/resizable", () => ({
  ResizablePanelGroup: ({ children }: { children: React.ReactNode }) => (
    <div>{children}</div>
  ),
  ResizablePanel: ({ children }: { children: React.ReactNode }) => (
    <div>{children}</div>
  ),
  ResizableHandle: () => null,
}));
// Same width-driven layout mock the inbox page tests use: the deep-link tests
// all want the desktop two-pane layout, the breakpoint tests at the bottom set
// their own width.
// iPhone Duo's inner screen: 669px wide held upright, 951px held sideways.
const FOLD_INNER_PORTRAIT = 669;
const FOLD_INNER_LANDSCAPE = 951;
const TABLET = 768;
const DESKTOP = 1440;
const layout = vi.hoisted(() => ({ width: 1440 }));
vi.mock("@multica/ui/hooks/use-mobile", () => ({
  useIsMobile: () => layout.width < 768,
  useIsCompact: () => layout.width < 768,
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
const platformWorkspace = vi.hoisted(() => ({ slug: "acme" }));
vi.mock("@multica/core/platform", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/platform")>()),
  getCurrentSlug: () => platformWorkspace.slug,
}));

const availableAgentsRef = vi.hoisted(() => ({ current: [] as Agent[] }));
const agentsSettledRef = vi.hoisted(() => ({ current: true }));
const interactiveLoginRequiredRef = vi.hoisted(() => ({ current: false }));
const mockStartNewChat = vi.hoisted(() => vi.fn());
const mockToastError = vi.hoisted(() => vi.fn());
// Spy over the REAL store's setter (wired in beforeEach). The page's effects
// run against a real Zustand store: a hand-rolled store mock cannot reproduce
// the ordering between a store update, the router publishing a URL, and the
// effects that reconcile the two (CHE-1755 ENG-11).
const mockSetActiveSession = vi.hoisted(() => vi.fn());

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: mockToastError },
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

vi.mock("./components/use-chat-controller", async () => {
  const { useChatStore } = await import("@multica/core/chat");
  return {
    useChatController: () => {
      const activeSession = useChatStore((state) => state.activeSessionId);
      return {
      wsId: "ws-1",
      user: { id: "user-1" },
      agents: availableAgentsRef.current,
      availableAgents: availableAgentsRef.current,
      agentsSettled: agentsSettledRef.current,
      sessions: [],
      interactiveLoginRequired: interactiveLoginRequiredRef.current,
      activeSessionId: activeSession,
      selectedAgentId: null,
      // session-9 is the DM whose transcript ends with a "continued into" link.
      currentSession: activeSession === "session-9"
        ? { id: "session-9", continued_into_session_id: "session-10" }
        : activeSession === "session-compacting"
          ? { id: "session-compacting", compacting: true }
          : activeSession === "session-empty"
            ? { id: "session-empty" }
            : null,
      isSessionArchived: false,
      isAgentArchived: false,
      isAgentRuntimeBound: true,
      activeAgent: availableAgentsRef.current[0] ?? null,
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
      handleRestoreDraftConsumed: vi.fn(),
      focusInputRequest: 0,
      handleSend: vi.fn(),
      handleStop: vi.fn(),
      handleUploadFile: vi.fn(),
      handleNewChat: () => mockSetActiveSession(null),
      handleStartNewChat: mockStartNewChat,
      // Like the real controller: selecting publishes the session to the store.
      handleSelectSession: (session: { id: string }) => mockSetActiveSession(session.id),
      advanceSelectionAfterArchive: vi.fn(),
      setActiveSession: mockSetActiveSession,
      setSelectedAgentId: vi.fn(),
      };
    },
  };
});

import { ChatPage } from "./chat-page";

const agent: Agent = {
  id: "agent-1",
  workspace_id: "ws-1",
  runtime_id: "runtime-1",
  name: "Lambda",
  description: "",
  instructions: "",
  avatar_url: null,
  runtime_mode: "local",
  runtime_config: {},
  custom_args: [],
  visibility: "workspace",
  permission_mode: "public_to",
  invocation_targets: [{ target_type: "workspace", target_id: null }],
  status: "idle",
  max_concurrent_tasks: 1,
  model: "",
  owner_id: "user-2",
  skills: [],
  created_at: "2026-05-28T00:00:00Z",
  updated_at: "2026-05-28T00:00:00Z",
  archived_at: null,
  archived_by: null,
};

const NO_ACCESS_MSG = "You don't have access to chat with this agent.";

function renderPage(search: string, { strict = false } = {}) {
  const replace = vi.fn();
  let navigation: NavigationAdapter = {
    push: vi.fn(),
    replace,
    back: vi.fn(),
    pathname: "/acme/chat",
    searchParams: new URLSearchParams(search),
    hash: "",
    getShareableUrl: (path) => path,
  };
  // A fresh element per render — reusing one element object lets React bail
  // out of re-rendering, which would make the rerender-based tests vacuous.
  // ChatPage reads the client-only quick-actions pending marker via useQuery,
  // so the page needs a QueryClient like it has in the real app shell.
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const makeUi = () => {
    const page = (
      <QueryClientProvider client={qc}>
        <I18nProvider locale="en" resources={TEST_RESOURCES}>
          <NavigationProvider value={navigation}>
            <ChatPage />
          </NavigationProvider>
        </I18nProvider>
      </QueryClientProvider>
    );
    return strict ? <StrictMode>{page}</StrictMode> : page;
  };
  const view = render(makeUi());
  return {
    replace,
    rerender: (
      { pathname, search }: { pathname?: string; search?: string } = {},
    ) => {
      navigation = {
        ...navigation,
        pathname: pathname ?? navigation.pathname,
        searchParams:
          search === undefined ? navigation.searchParams : new URLSearchParams(search),
      };
      view.rerender(makeUi());
    },
  };
}

// Seed the real store without going through the spied setter.
const seedSession = (id: string | null) =>
  useChatStore.setState({ activeSessionId: id });
const activeSession = () => useChatStore.getState().activeSessionId;

beforeEach(() => {
  interactiveLoginRequiredRef.current = false;
  vi.clearAllMocks();
  mockSetActiveSession.mockImplementation((id: string | null) =>
    useChatStore.getState().setActiveSession(id),
  );
  seedSession(null);
  availableAgentsRef.current = [agent];
  agentsSettledRef.current = true;
  platformWorkspace.slug = "acme";
  layout.width = DESKTOP;
});

describe("ChatPage URL synchronization", () => {
  it("hands ?message= to the list for the session it names, then drops it once landed", () => {
    seedSession("session-9");
    const { replace } = renderPage("session=session-9&message=msg-7");
    expect(screen.getByTestId("jump-target")).toHaveTextContent("msg-7");
    fireEvent.click(screen.getByText("settle-landed"));
    expect(replace).toHaveBeenCalledWith("/acme/chat?session=session-9");
    expect(mockToastError).not.toHaveBeenCalled();
  });

  it("toasts and drops ?message= when the message cannot be found", () => {
    seedSession("session-9");
    const { replace } = renderPage("session=session-9&message=gone");
    fireEvent.click(screen.getByText("settle-missing"));
    expect(mockToastError).toHaveBeenCalledWith("Message no longer available");
    expect(replace).toHaveBeenCalledWith("/acme/chat?session=session-9");
  });

  it("toasts and drops ?message= when the chat has no messages left to show", () => {
    seedSession("session-empty");
    const { replace } = renderPage("session=session-empty&message=gone");
    expect(screen.getByText("chat-empty-state")).toBeInTheDocument();
    expect(mockToastError).toHaveBeenCalledWith("Message no longer available");
    expect(replace).toHaveBeenCalledWith("/acme/chat?session=session-empty");
  });

  it("settles an empty chat's missing message once under StrictMode", () => {
    seedSession("session-empty");
    renderPage("session=session-empty&message=gone", { strict: true });
    expect(mockToastError).toHaveBeenCalledTimes(1);
  });

  it("leaves a session with no ?message= alone when it is empty", () => {
    seedSession("session-empty");
    const { replace } = renderPage("session=session-empty");
    expect(mockToastError).not.toHaveBeenCalled();
    expect(replace).not.toHaveBeenCalled();
  });

  it("passes no jump target without ?message=", () => {
    seedSession("session-9");
    renderPage("session=session-9");
    expect(screen.getByTestId("jump-target")).toHaveTextContent("");
  });


  it("prompts for interactive re-login without navigating or clearing the session", () => {
    interactiveLoginRequiredRef.current = true;
    seedSession("preserved-session");
    const page = renderPage("?session=preserved-session");
    expect(screen.getByRole("alert")).toHaveTextContent("Sign in again to use Chat");
    expect(activeSession()).toBe("preserved-session");
    expect(page.replace).not.toHaveBeenCalled();
    expect(mockSignIn).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(mockSignIn).toHaveBeenCalledTimes(1);
  });
  it.each(["/globex/issues", "/acme/issues"])(
    "does not redirect back to chat when the retained page observes %s",
    (pathname) => {
      const { replace, rerender } = renderPage("session=session-1");
      expect(activeSession()).toBe("session-1");
      mockSetActiveSession.mockClear();

      // App Router can retain the outgoing page after the shared navigation
      // adapter has already published the destination URL.
      rerender({ pathname, search: "" });

      expect(replace).not.toHaveBeenCalled();
      expect(mockSetActiveSession).not.toHaveBeenCalled();
    },
  );

  it.each([null, "session-3"])(
    "does not write the rehydrated session %s into the outgoing workspace's URL",
    (sessionId) => {
      const { replace, rerender } = renderPage("session=session-1");
      rerender({ pathname: "/globex/issues" });
      mockSetActiveSession.mockClear();
      replace.mockClear();

      act(() => {
        mockSetActiveSession(sessionId);
      });

      expect(activeSession()).toBe(sessionId);
      expect(mockSetActiveSession).toHaveBeenCalledTimes(1);
      expect(replace).not.toHaveBeenCalled();
    },
  );

  it.each([null, "session-3"])(
    "ignores rehydration to %s before the destination pathname commits",
    (sessionId) => {
      const { replace } = renderPage("session=session-1");
      mockSetActiveSession.mockClear();

      // The incoming layout can rehydrate the shared store while the
      // outgoing page's navigation adapter still reports /acme/chat.
      platformWorkspace.slug = "globex";
      act(() => {
        mockSetActiveSession(sessionId);
      });

      expect(activeSession()).toBe(sessionId);
      expect(mockSetActiveSession).toHaveBeenCalledTimes(1);
      expect(replace).not.toHaveBeenCalled();
    },
  );

  it("resumes synchronization when returning to the same chat URL", () => {
    const { replace, rerender } = renderPage("session=session-1");
    rerender({ pathname: "/globex/issues", search: "session=session-1" });
    act(() => {
      mockSetActiveSession("session-2");
    });
    replace.mockClear();
    mockSetActiveSession.mockClear();

    rerender({ pathname: "/acme/chat" });

    expect(mockSetActiveSession).toHaveBeenCalledWith("session-1");
    expect(activeSession()).toBe("session-1");
    expect(replace).not.toHaveBeenCalled();
  });

  it("mirrors thread selection and clearing while the chat route is current", () => {
    const { replace } = renderPage("session=session-1");

    act(() => {
      mockSetActiveSession("session-2");
    });
    expect(replace).toHaveBeenLastCalledWith("/acme/chat?session=session-2");

    act(() => {
      mockSetActiveSession(null);
    });
    expect(replace).toHaveBeenLastCalledWith("/acme/chat");
  });

  it.each(["pathname", "rehydration"])(
    "does not consume a pending agent link after workspace switching starts via %s",
    (transition) => {
      availableAgentsRef.current = [];
      agentsSettledRef.current = false;
      const { replace, rerender } = renderPage("agent=agent-1");

      availableAgentsRef.current = [agent];
      agentsSettledRef.current = true;
      if (transition === "rehydration") platformWorkspace.slug = "globex";
      rerender({
        pathname: transition === "pathname" ? "/globex/chat" : "/acme/chat",
      });

      expect(mockStartNewChat).not.toHaveBeenCalled();
      expect(mockToastError).not.toHaveBeenCalled();
      expect(replace).not.toHaveBeenCalled();
    },
  );
});

// CHE-1807: while a DM compacts, the page's composer is disabled and says why.
describe("ChatPage compacting composer", () => {
  it("disables the composer for a compacting DM and leaves other DMs alone", () => {
    seedSession("session-compacting");
    renderPage("session=session-compacting");
    expect(screen.getByText("chat-input")).toHaveAttribute("data-disabled", "true");
    expect(screen.getByText("chat-input")).toHaveAttribute("data-compacting", "true");
  });

  it("keeps the composer enabled for a DM that is not compacting", () => {
    seedSession("session-9");
    renderPage("session=session-9");
    expect(screen.getByText("chat-input")).toHaveAttribute("data-disabled", "false");
    expect(screen.getByText("chat-input")).toHaveAttribute("data-compacting", "false");
  });
});

describe("ChatPage ?agent= deep link", () => {
  it("starts a new chat with the linked agent and strips the param", () => {
    const history = createHistory("/acme/chat?agent=agent-1");
    renderWithHistory(history);
    expect(mockStartNewChat).toHaveBeenCalledTimes(1);
    expect(mockStartNewChat).toHaveBeenCalledWith(agent);
    expect(history.url).toBe("/acme/chat?new=1");
    // The URL opens the conversation pane instead of the neutral prompt.
    expect(screen.getByText("chat-input")).toBeInTheDocument();
  });

  it("consumes the intent only once even while the param is still in the URL", () => {
    // navigation.replace is async in real adapters — the param outlives the
    // consuming render. The real controller also allocates a fresh
    // availableAgents array every render (it filters), so the effect re-runs;
    // only the consumed-intent guard stops a second chat from starting.
    const { rerender } = renderPage("agent=agent-1");
    availableAgentsRef.current = [agent];
    rerender();
    expect(mockStartNewChat).toHaveBeenCalledTimes(1);
  });

  it("stays pending without a toast while the agent/member queries load", () => {
    availableAgentsRef.current = [];
    agentsSettledRef.current = false;
    const { replace } = renderPage("agent=agent-1");
    expect(mockStartNewChat).not.toHaveBeenCalled();
    expect(mockToastError).not.toHaveBeenCalled();
    expect(replace).not.toHaveBeenCalled();
  });

  it("waits for the agent list to resolve before consuming the intent", () => {
    availableAgentsRef.current = [];
    agentsSettledRef.current = false;
    const { replace, rerender } = renderPage("agent=agent-1");
    expect(mockStartNewChat).not.toHaveBeenCalled();
    availableAgentsRef.current = [agent];
    agentsSettledRef.current = true;
    rerender();
    expect(mockStartNewChat).toHaveBeenCalledWith(agent);
    expect(replace).toHaveBeenCalledWith("/acme/chat?new=1");
  });

  it("toasts and strips the param once the list settles without the agent", () => {
    // Revoked access, archived agent, or a bad id: a settled miss must
    // explain itself and consume the intent so a later refetch that surfaces
    // the agent cannot auto-start a chat without a fresh click.
    const { replace, rerender } = renderPage("agent=other-agent");
    expect(mockStartNewChat).not.toHaveBeenCalled();
    expect(mockToastError).toHaveBeenCalledWith(NO_ACCESS_MSG);
    expect(replace).toHaveBeenCalledWith("/acme/chat");
    expect(screen.queryByText("chat-input")).not.toBeInTheDocument();
    rerender();
    expect(mockToastError).toHaveBeenCalledTimes(1);
  });

  it("lets an explicit thread selection supersede a still-pending intent", () => {
    // Deferred-intent race (review P1): the user picks a thread while the
    // agent/member queries are still loading; when they settle, the stale
    // deep link must NOT fire and clobber that selection.
    availableAgentsRef.current = [];
    agentsSettledRef.current = false;
    const { replace, rerender } = renderPage("agent=agent-1");
    fireEvent.click(screen.getByRole("button", { name: "select-thread" }));
    availableAgentsRef.current = [agent];
    agentsSettledRef.current = true;
    rerender();
    expect(mockStartNewChat).not.toHaveBeenCalled();
    expect(mockToastError).not.toHaveBeenCalled();
    expect(replace).not.toHaveBeenCalled();
  });

  it("opens the compose pane under StrictMode with a persisted previous session", () => {
    // StrictMode replays mount effects with render-captured values (review
    // P2): the composingNew reset must read the LIVE store value, or the
    // stale persisted session re-closes the pane the intent just opened —
    // while the consumed-intent guard rightly refuses to fire again.
    seedSession("old-session");
    const history = createHistory("/acme/chat?agent=agent-1");
    renderWithHistory(history, { strict: true });
    expect(mockStartNewChat).toHaveBeenCalledTimes(1);
    expect(history.url).toBe("/acme/chat?new=1");
    expect(screen.getByText("chat-input")).toBeInTheDocument();
  });
});

describe("ChatPage responsive layout", () => {
  const SELECT_PROMPT = "Pick a conversation, or start a new one with +";

  it("folds to a single column on a foldable's inner screen held upright", () => {
    // Below the compact breakpoint, with nothing open, the page spends the
    // whole width on the thread list instead of an empty conversation pane.
    layout.width = FOLD_INNER_PORTRAIT;
    renderPage("");

    expect(
      screen.getByRole("button", { name: "select-thread" }),
    ).toBeInTheDocument();
    expect(screen.queryByText(SELECT_PROMPT)).not.toBeInTheDocument();
  });

  it("gives the open conversation the whole upright inner screen", () => {
    // Opening a thread replaces the list, so the way back has to travel with
    // the conversation — same trip the phone layout already had.
    layout.width = FOLD_INNER_PORTRAIT;
    // The page reconciles the store against `?session=`, so the open thread has
    // to be in the URL too — otherwise the sync effect closes it.
    seedSession("session-1");
    renderPage("session=session-1");

    expect(
      screen.queryByRole("button", { name: "select-thread" }),
    ).not.toBeInTheDocument();
    expect(screen.getByText("chat-input")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Chat" })).toBeInTheDocument();
  });

  it.each([TABLET, FOLD_INNER_LANDSCAPE])("keeps both panes at %spx", (width) => {
    // 768px is the first width that keeps two panes. The nav auto-collapses
    // there instead (see the sidebar), so the thread list stays on screen next
    // to an open conversation.
    layout.width = width;
    seedSession("session-1");
    renderPage("session=session-1");

    expect(
      screen.getByRole("button", { name: "select-thread" }),
    ).toBeInTheDocument();
    expect(screen.getByText("chat-input")).toBeInTheDocument();
  });
});

// A real history stack behind the adapter, so these tests assert where Back
// lands rather than which adapter method was called. `/acme/inbox` is the
// start destination Cheese opened chat from. `writes` records only what the
// page itself pushed or replaced; a test simulating an AppLink calls
// `history.push` directly and stays out of it. `defer()` holds back the URL
// the page can read, like a router that publishes a push after the store has
// already changed.
function createHistory(...entries: string[]) {
  let index = entries.length - 1;
  let published = entries[index]!;
  let deferred = false;
  const listeners = new Set<() => void>();
  const publish = () => {
    if (deferred) return;
    published = entries[index]!;
    listeners.forEach((l) => l());
  };
  return {
    writes: [] as { kind: "push" | "replace"; url: string }[],
    get entries() {
      return entries;
    },
    get url() {
      return entries[index]!;
    },
    get publishedUrl() {
      return published;
    },
    subscribe: (cb: () => void) => {
      listeners.add(cb);
      return () => listeners.delete(cb);
    },
    defer() {
      deferred = true;
    },
    flush() {
      deferred = false;
      publish();
    },
    push(url: string) {
      entries = [...entries.slice(0, index + 1), url];
      index = entries.length - 1;
      publish();
    },
    replace(url: string) {
      entries = entries.map((e, i) => (i === index ? url : e));
      publish();
    },
    back() {
      if (index > 0) index -= 1;
      publish();
    },
    forward() {
      if (index < entries.length - 1) index += 1;
      publish();
    },
    canGoBack: () => index > 0,
  };
}

function renderWithHistory(
  history: ReturnType<typeof createHistory>,
  { strict = false, navigationApi = true } = {},
) {
  let bump: () => void = () => {};
  function Host() {
    const [, setTick] = useState(0);
    bump = () => setTick((n) => n + 1);
    const url = useSyncExternalStore(history.subscribe, () => history.publishedUrl);
    const [pathname = "", search = ""] = url.split("?");
    const navigation: NavigationAdapter = {
      push: (to) => {
        history.writes.push({ kind: "push", url: to });
        history.push(to);
      },
      replace: (to) => {
        history.writes.push({ kind: "replace", url: to });
        history.replace(to);
      },
      back: history.back,
      // Browsers without the Navigation API cannot answer, so the adapter omits it.
      canGoBack: navigationApi ? history.canGoBack : undefined,
      pathname,
      searchParams: new URLSearchParams(search),
      hash: "",
      getShareableUrl: (path) => path,
    };
    const page = (
      <QueryClientProvider client={new QueryClient()}>
        <I18nProvider locale="en" resources={TEST_RESOURCES}>
          <NavigationProvider value={navigation}>
            <ChatPage />
          </NavigationProvider>
        </I18nProvider>
      </QueryClientProvider>
    );
    return strict ? <StrictMode>{page}</StrictMode> : page;
  }
  const view = render(<Host />);
  return { ...view, rerenderPage: () => act(() => bump()) };
}

describe("ChatPage back stack", () => {
  const selectThread = () =>
    fireEvent.click(screen.getByRole("button", { name: "select-thread" }));
  const back = () => act(() => history.back());
  let history: ReturnType<typeof createHistory>;

  it("walks inbox > list > A > linked B back through A, the list and the inbox", () => {
    history = createHistory("/acme/inbox", "/acme/chat");
    renderWithHistory(history);

    selectThread();
    expect(history.url).toBe("/acme/chat?session=session-9");
    // The "continued from" divider link pushes through AppLink.
    fireEvent.click(screen.getByRole("link", { name: "continued-link" }));
    expect(activeSession()).toBe("session-10");

    back();
    expect(history.url).toBe("/acme/chat?session=session-9");
    expect(activeSession()).toBe("session-9");
    back();
    expect(history.url).toBe("/acme/chat");
    expect(activeSession()).toBeNull();
    back();
    expect(history.url).toBe("/acme/inbox");
  });

  it("opens a message hit with one pushed entry that targets the message, and Back returns to the list", () => {
    history = createHistory("/acme/inbox", "/acme/chat");
    renderWithHistory(history);

    fireEvent.click(screen.getByRole("button", { name: "open-message-hit" }));

    expect(history.writes).toEqual([
      { kind: "push", url: "/acme/chat?session=session-9&message=msg-4" },
    ]);
    expect(activeSession()).toBe("session-9");
    expect(screen.getByTestId("jump-target")).toHaveTextContent("msg-4");

    // Landing drops ?message= by replace, so the entry count stays at one push.
    fireEvent.click(screen.getByRole("button", { name: "settle-landed" }));
    expect(history.entries).toEqual([
      "/acme/inbox",
      "/acme/chat",
      "/acme/chat?session=session-9",
    ]);

    back();
    expect(history.url).toBe("/acme/chat");
    expect(activeSession()).toBeNull();
  });

  it("pushes a thread selection instead of replacing the list entry", () => {
    history = createHistory("/acme/inbox", "/acme/chat");
    renderWithHistory(history);

    selectThread();

    expect(history.entries).toEqual([
      "/acme/inbox",
      "/acme/chat",
      "/acme/chat?session=session-9",
    ]);
  });

  it("pushes ?new=1 when a new chat is started from an open session", () => {
    history = createHistory("/acme/inbox", "/acme/chat?session=session-9");
    seedSession("session-9");
    renderWithHistory(history);

    fireEvent.click(screen.getByRole("button", { name: "new-chat" }));

    expect(history.entries).toEqual([
      "/acme/inbox",
      "/acme/chat?session=session-9",
      "/acme/chat?new=1",
    ]);
    back();
    expect(history.url).toBe("/acme/chat?session=session-9");
    expect(activeSession()).toBe("session-9");
  });

  it("assigns the server ID of a new chat with replace, not push", () => {
    history = createHistory("/acme/inbox", "/acme/chat");
    renderWithHistory(history);

    act(() => mockSetActiveSession("new-session"));

    expect(history.entries).toEqual(["/acme/inbox", "/acme/chat?session=new-session"]);
  });

  // Corrections from the CHE-1755 /autoplan review (2026-10-16). They assert
  // history and URL writes, not which call site or store field produced them,
  // so they hold whichever push/replace mechanism Cheese picks on CHE-1755.
  // TE2 (CHE-1755 D5): the page asks the store for a push or replace, and one
  // store -> URL effect writes it. These tests watch the real store's intent.
  describe("one-shot navIntent", () => {
    const watchIntents = () => {
      const seen: unknown[] = [];
      const unsubscribe = useChatStore.subscribe((state, prev) => {
        if (state.navIntent !== prev.navIntent) seen.push(state.navIntent);
      });
      return { seen, unsubscribe };
    };

    it("records a push for a thread selection and consumes it once the URL is written", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history, { strict: true });
      const { seen, unsubscribe } = watchIntents();

      selectThread();
      unsubscribe();

      expect(seen).toEqual([{ mode: "push", newChat: false }, null]);
      expect(history.writes).toEqual([
        { kind: "push", url: "/acme/chat?session=session-9" },
      ]);
    });

    it("records a push for a new chat", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history, { strict: true });
      const { seen, unsubscribe } = watchIntents();

      fireEvent.click(screen.getByRole("button", { name: "new-chat" }));
      unsubscribe();

      expect(seen).toEqual([{ mode: "push", newChat: true }, null]);
    });

    it("records a replace for a resolved ?agent= intent", () => {
      availableAgentsRef.current = [agent];
      history = createHistory("/acme/inbox", "/acme/chat?agent=agent-1");
      const { seen, unsubscribe } = watchIntents();

      renderWithHistory(history, { strict: true });
      unsubscribe();

      expect(seen).toEqual([{ mode: "replace", newChat: true }, null]);
    });

    it("records no intent when the open DM is selected again", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history, { strict: true });
      selectThread();
      const { seen, unsubscribe } = watchIntents();

      selectThread();
      unsubscribe();

      expect(seen).toEqual([]);
      expect(useChatStore.getState().navIntent).toBeNull();
    });

    it("writes any store change that carries a push intent as exactly one push", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history, { strict: true });

      // A caller outside the page's handlers, such as a future slash command.
      act(() => {
        useChatStore.getState().requestNav({ mode: "push", newChat: false });
        useChatStore.getState().setActiveSession("session-12");
      });

      expect(history.writes).toEqual([
        { kind: "push", url: "/acme/chat?session=session-12" },
      ]);
    });

    it("turns the next store-only change after a consumed intent into a replace", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history, { strict: true });
      selectThread();

      act(() => mockSetActiveSession("session-10"));

      expect(history.writes).toEqual([
        { kind: "push", url: "/acme/chat?session=session-9" },
        { kind: "replace", url: "/acme/chat?session=session-10" },
      ]);
    });

    it("writes one push under StrictMode when the router publishes after the store changes", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history, { strict: true });
      history.defer();

      selectThread();
      expect(activeSession()).toBe("session-9");
      act(() => history.flush());

      expect(history.writes).toEqual([
        { kind: "push", url: "/acme/chat?session=session-9" },
      ]);
    });
  });

  it("returns to DM A when back follows an issue link opened from it", () => {
    history = createHistory("/acme/inbox", "/acme/chat");
    renderWithHistory(history);
    selectThread();

    act(() => history.push("/acme/issues/MUL-1"));
    back();

    expect(history.url).toBe("/acme/chat?session=session-9");
    expect(activeSession()).toBe("session-9");
  });

  describe("one URL write per user transition", () => {
    it("pushes a list > DM selection once and replaces nothing under StrictMode", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history, { strict: true });

      selectThread();

      expect(history.writes).toEqual([
        { kind: "push", url: "/acme/chat?session=session-9" },
      ]);
    });

    it("does not replace the entry a push is about to create when the router publishes late", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history);
      history.defer();

      selectThread();
      // Store already says session-9; the router still shows the list.
      expect(activeSession()).toBe("session-9");
      act(() => history.flush());

      expect(history.writes).toEqual([
        { kind: "push", url: "/acme/chat?session=session-9" },
      ]);
      expect(history.entries).toEqual([
        "/acme/inbox",
        "/acme/chat",
        "/acme/chat?session=session-9",
      ]);
    });

    it("adds no history entry when the open DM is selected again", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history);
      selectThread();
      const entries = [...history.entries];

      selectThread();

      expect(history.entries).toEqual(entries);
      expect(history.writes).toHaveLength(1);
    });

    it("writes nothing to the URL on browser back and forward", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history);
      selectThread();
      fireEvent.click(screen.getByRole("link", { name: "continued-link" }));
      const writes = [...history.writes];

      back();
      back();

      expect(history.writes).toEqual(writes);
      expect(activeSession()).toBeNull();
    });

    it.each([
      // Realtime continuation advancing the open session, a stale or deleted
      // session being cleared: store-only writers, no user transition.
      ["advances to a continued session", "session-10", "/acme/chat?session=session-10"],
      ["clears a stale or deleted session", null, "/acme/chat"],
    ])("writes the URL exactly once when the store %s", (_label, next, url) => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history);
      selectThread();
      history.writes.length = 0;

      act(() => mockSetActiveSession(next));

      expect(history.writes).toEqual([{ kind: "replace", url }]);
      expect(history.url).toBe(url);
    });
  });

  describe("router publishes late", () => {
    it("pushes ?new=1 once and never replaces it with the list when started from a DM", () => {
      history = createHistory("/acme/inbox", "/acme/chat?session=session-9");
      seedSession("session-9");
      renderWithHistory(history, { strict: true });
      history.defer();

      fireEvent.click(screen.getByRole("button", { name: "new-chat" }));
      act(() => history.flush());

      expect(history.writes).toEqual([{ kind: "push", url: "/acme/chat?new=1" }]);
      expect(history.entries).toEqual([
        "/acme/inbox",
        "/acme/chat?session=session-9",
        "/acme/chat?new=1",
      ]);
    });

    it("pushes ?new=1 once when started from the list", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history, { strict: true });
      history.defer();

      fireEvent.click(screen.getByRole("button", { name: "new-chat" }));
      act(() => history.flush());

      expect(history.writes).toEqual([{ kind: "push", url: "/acme/chat?new=1" }]);
    });

    it("replaces ?agent= with ?new=1 once", () => {
      history = createHistory("/acme/inbox", "/acme/chat?agent=agent-1");
      history.defer();
      renderWithHistory(history, { strict: true });
      act(() => history.flush());

      expect(history.writes).toEqual([{ kind: "replace", url: "/acme/chat?new=1" }]);
    });

    it("keeps the compose pane tied to the published URL", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history);
      history.defer();

      fireEvent.click(screen.getByRole("button", { name: "new-chat" }));
      expect(screen.queryByText("chat-input")).not.toBeInTheDocument();
      act(() => history.flush());

      expect(screen.getByText("chat-input")).toBeInTheDocument();
    });
  });

  describe("unsent new chat", () => {
    it("pushes ?new=1 once when a new chat is started from the list", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history, { strict: true });

      fireEvent.click(screen.getByRole("button", { name: "new-chat" }));

      expect(history.writes).toEqual([{ kind: "push", url: "/acme/chat?new=1" }]);
      back();
      expect(history.url).toBe("/acme/chat");
    });

    it("assigns the server ID over ?new=1 with one replace", () => {
      history = createHistory("/acme/inbox", "/acme/chat?new=1");
      renderWithHistory(history);

      act(() => mockSetActiveSession("new-session"));

      expect(history.writes).toEqual([
        { kind: "replace", url: "/acme/chat?session=new-session" },
      ]);
      expect(history.entries).toEqual(["/acme/inbox", "/acme/chat?session=new-session"]);
    });

    it("turns a ?agent= deep link into ?new=1 by replace, with no push", () => {
      availableAgentsRef.current = [agent];
      history = createHistory("/acme/inbox", "/acme/chat?agent=agent-1");
      renderWithHistory(history, { strict: true });

      expect(mockStartNewChat).toHaveBeenCalledTimes(1);
      expect(history.writes.filter((w) => w.kind === "push")).toEqual([]);
      expect(history.entries).toEqual(["/acme/inbox", "/acme/chat?new=1"]);
    });
  });

  describe("compact back button", () => {
    beforeEach(() => {
      layout.width = FOLD_INNER_PORTRAIT;
    });

    it("steps back to the previous session, then to the list", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history);
      selectThread();
      fireEvent.click(screen.getByRole("link", { name: "continued-link" }));

      fireEvent.click(screen.getByRole("button", { name: "Chat" }));
      expect(history.url).toBe("/acme/chat?session=session-9");
      fireEvent.click(screen.getByRole("button", { name: "Chat" }));
      expect(history.url).toBe("/acme/chat");
      expect(screen.getByRole("button", { name: "select-thread" })).toBeInTheDocument();
    });

    it("walks the repro with the back button, then browser back reaches the inbox", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history);
      selectThread();
      fireEvent.click(screen.getByRole("link", { name: "continued-link" }));

      fireEvent.click(screen.getByRole("button", { name: "Chat" }));
      expect(history.url).toBe("/acme/chat?session=session-9");
      fireEvent.click(screen.getByRole("button", { name: "Chat" }));
      expect(history.url).toBe("/acme/chat");
      back();

      expect(history.url).toBe("/acme/inbox");
    });

    it("falls back to the list when the browser cannot report canGoBack", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history, { navigationApi: false });
      selectThread();

      fireEvent.click(screen.getByRole("button", { name: "Chat" }));

      expect(history.url).toBe("/acme/chat");
      expect(activeSession()).toBeNull();
      expect(screen.getByRole("button", { name: "select-thread" })).toBeInTheDocument();
    });

    it("keeps the compose pane when ?new=1 is opened cold", () => {
      history = createHistory("/acme/chat?new=1");
      renderWithHistory(history);

      expect(screen.getByText("chat-input")).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "select-thread" })).not.toBeInTheDocument();
    });

    it("keeps the compose pane on a refresh of ?new=1 that has a persisted session", () => {
      seedSession("persisted-session");
      history = createHistory("/acme/chat?new=1");
      renderWithHistory(history);

      expect(activeSession()).toBeNull();
      expect(history.url).toBe("/acme/chat?new=1");
      expect(screen.getByText("chat-input")).toBeInTheDocument();
    });

    it("shows the compose pane again on forward after back from an unsent new chat", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history);
      fireEvent.click(screen.getByRole("button", { name: "new-chat" }));
      back();
      const writes = [...history.writes];

      act(() => history.forward());

      expect(history.url).toBe("/acme/chat?new=1");
      expect(screen.getByText("chat-input")).toBeInTheDocument();
      expect(history.writes).toEqual(writes);
    });

    it("lands on the list from a cold ?new=1 with the back button", () => {
      history = createHistory("/acme/chat?new=1");
      renderWithHistory(history);

      fireEvent.click(screen.getByRole("button", { name: "Chat" }));

      expect(history.entries).toEqual(["/acme/chat"]);
      expect(screen.getByRole("button", { name: "select-thread" })).toBeInTheDocument();
    });

    it("returns to the list on browser back from an unsent new chat", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history);
      fireEvent.click(screen.getByRole("button", { name: "new-chat" }));
      expect(screen.getByText("chat-input")).toBeInTheDocument();

      back();

      expect(history.url).toBe("/acme/chat");
      expect(screen.getByRole("button", { name: "select-thread" })).toBeInTheDocument();
      expect(screen.queryByText("chat-input")).not.toBeInTheDocument();
    });

    it("returns to the list on the back button from an unsent new chat", () => {
      history = createHistory("/acme/inbox", "/acme/chat");
      renderWithHistory(history);
      fireEvent.click(screen.getByRole("button", { name: "new-chat" }));

      fireEvent.click(screen.getByRole("button", { name: "Chat" }));

      expect(history.url).toBe("/acme/chat");
      expect(screen.getByRole("button", { name: "select-thread" })).toBeInTheDocument();
    });

    it("lands on the list, not outside Multica, after a cold open of a session", () => {
      history = createHistory("/acme/chat?session=session-10");
      seedSession("session-10");
      renderWithHistory(history);

      fireEvent.click(screen.getByRole("button", { name: "Chat" }));

      expect(history.entries).toEqual(["/acme/chat"]);
      expect(activeSession()).toBeNull();
    });
  });
});

describe("ChatPage deep link vs workspace rehydration", () => {
  // The [workspaceSlug] layout publishes the workspace in an effect, and the
  // persisted chat store rehydrates from that workspace's namespace one
  // microtask later. When the page mounts in the same commit as that effect,
  // the rehydration lands after the page's own URL -> store sync and replaced
  // the selection the URL names (CHE-1900: the quick-actions E2E deep link
  // ended on "Pick a conversation" with `?session=` stripped).
  const publishWorkspace = async () => {
    await act(async () => {
      setCurrentWorkspace("acme", "ws-1");
      await Promise.resolve();
    });
  };

  beforeEach(async () => {
    setCurrentWorkspace(null, null);
    // Leaving a workspace rehydrates from the un-namespaced key, which an
    // earlier test's selection wrote; clear it through the store's own setter.
    await Promise.resolve();
    useChatStore.getState().setActiveSession(null);
  });

  // The pages here outlive the test that rendered them unless unmounted, and a
  // retained page would keep reacting to the next test's workspace changes.
  afterEach(cleanup);

  it("keeps the thread the URL names when the rehydration lands after the mount sync", async () => {
    const { replace } = renderPage("session=session-5");
    expect(activeSession()).toBe("session-5");

    await publishWorkspace();

    expect(activeSession()).toBe("session-5");
    expect(replace).not.toHaveBeenCalled();
  });

  it("applies the URL thread when the workspace only becomes current after the mount", async () => {
    platformWorkspace.slug = null as unknown as string;
    const { replace } = renderPage("session=session-5");
    expect(activeSession()).toBeNull();

    platformWorkspace.slug = "acme";
    await publishWorkspace();

    expect(activeSession()).toBe("session-5");
    expect(replace).not.toHaveBeenCalled();
  });
});
