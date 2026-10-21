import { act, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { AnimatePresence } from "motion/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { chatKeys } from "@multica/core/chat/queries";
import { ApiError } from "@multica/core/api";
import type { ChatMessage, ChatSession } from "@multica/core/types";
import { ChatWindowMessages } from "./chat-window-messages";

const fetchMessages = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: { listChatMessagesPage: fetchMessages },
}));
vi.mock("./interactive-login-banner", () => ({
  InteractiveLoginBanner: () => <div data-testid="login-banner" />,
}));
vi.mock("@multica/core/chat/mutations", () => ({
  useRegenerateChatQuickActions: () => ({ mutateAsync: vi.fn() }),
}));
vi.mock("./chat-message-list", () => ({
  ChatMessageList: ({ active, messages, continuedFromSessionId }: { active: boolean; messages: ChatMessage[]; continuedFromSessionId?: string | null }) => (
    <div data-testid="messages" data-active={active} data-continued-from={continuedFromSessionId ?? ""}>
      {messages.map((message) => <p key={message.id}>{message.content}</p>)}
    </div>
  ),
  ChatMessageSkeleton: () => <div data-testid="skeleton" />,
}));

function message(id: string): ChatMessage {
  return {
    id, chat_session_id: "session-1", role: "user", content: id,
    task_id: null, created_at: "2026-10-20T00:00:00Z",
  };
}

function pages(messages: ChatMessage[]) {
  return { pages: [{ messages, limit: 50, has_more: false }], pageParams: [null] };
}

function renderWindow(
  open: boolean,
  opts: {
    initial?: ChatMessage[];
    session?: Partial<ChatSession>;
    interactiveLoginRequired?: boolean;
    seed?: (qc: QueryClient, key: readonly unknown[]) => void;
    onMessages?: (messages: ChatMessage[]) => void;
  } = {},
) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const key = chatKeys.messagesPage("session-1");
  if (opts.seed) opts.seed(qc, key);
  else qc.setQueryData(key, pages(opts.initial ?? [message("initial message")]));
  const content = (isOpen: boolean) => (
    <QueryClientProvider client={qc}>
      <AnimatePresence initial={false}>
        {isOpen && (
          <ChatWindowMessages
            sessionId="session-1"
            session={opts.session as ChatSession | undefined}
            interactiveLoginRequired={opts.interactiveLoginRequired ?? false}
            onMessages={opts.onMessages}
            pendingTask={undefined}
            pendingTaskLoading={false}
            availability={undefined}
            onQuickAction={vi.fn()}
            quickActionsDisabled={false}
            quickActionsPendingMessageId={null}
          >
            <div>empty</div>
          </ChatWindowMessages>
        )}
      </AnimatePresence>
    </QueryClientProvider>
  );
  const view = render(content(open));
  return {
    qc,
    key,
    setOpen: (isOpen: boolean) => view.rerender(content(isOpen)),
    observers: () => qc.getQueryCache().find({ queryKey: key })?.getObserversCount() ?? 0,
  };
}

beforeEach(() => { fetchMessages.mockReset(); });

describe("floating chat message lifetime", () => {
  it("keeps message observers absent when the window starts closed", () => {
    const { observers } = renderWindow(false);
    expect(screen.queryByTestId("messages")).not.toBeInTheDocument();
    expect(observers()).toBe(0);
    expect(fetchMessages).not.toHaveBeenCalled();
  });

  it("follows immediately on open, stops during exit, then reads new data on reopen", async () => {
    const { qc, key, setOpen, observers } = renderWindow(true);
    expect(screen.getByTestId("messages")).toHaveAttribute("data-active", "true");
    expect(observers()).toBe(1);

    setOpen(false);
    expect(screen.getByTestId("messages")).toHaveAttribute("data-active", "false");
    await waitFor(() => expect(screen.queryByTestId("messages")).not.toBeInTheDocument());
    // The observer unsubscribes in a passive effect cleanup, which can run
    // after the node has left the DOM.
    await waitFor(() => expect(observers()).toBe(0));

    act(() => qc.setQueryData(key, pages([message("arrived while closed")])));
    setOpen(true);
    expect(screen.getByText("arrived while closed")).toBeInTheDocument();
    expect(screen.getByTestId("messages")).toHaveAttribute("data-active", "true");
    expect(observers()).toBe(1);
  });

  it("cancels an interrupted exit without losing the live list", async () => {
    const { setOpen, observers } = renderWindow(true);
    const list = screen.getByTestId("messages");
    setOpen(false);
    setOpen(true);
    expect(screen.getByTestId("messages")).toBe(list);
    expect(list).toHaveAttribute("data-active", "true");
    await waitFor(() => expect(list.parentElement?.style.opacity).toBe("1"));
    expect(observers()).toBe(1);
  });

  it("fetches the full message window when its inactive cache was collected", async () => {
    const { qc, key, setOpen, observers } = renderWindow(true);
    setOpen(false);
    await waitFor(() => expect(screen.queryByTestId("messages")).not.toBeInTheDocument());
    await waitFor(() => expect(observers()).toBe(0));
    qc.removeQueries({ queryKey: key });
    fetchMessages.mockResolvedValue({ messages: [message("refetched after collection")], limit: 50, has_more: false });

    setOpen(true);

    expect(await screen.findByText("refetched after collection")).toBeInTheDocument();
    expect(fetchMessages).toHaveBeenCalledTimes(1);
  });
});

describe("fork session links and interactive login", () => {
  it("shows the list for an empty session that continues another one", () => {
    renderWindow(true, { initial: [], session: { continued_from_session_id: "older" } });
    expect(screen.getByTestId("messages")).toHaveAttribute("data-continued-from", "older");
    expect(screen.queryByText("empty")).not.toBeInTheDocument();
  });

  it("shows the empty state for an empty session with no links", () => {
    renderWindow(true, { initial: [] });
    expect(screen.getByText("empty")).toBeInTheDocument();
  });

  it("shows the login banner instead of the list when sessions need an interactive login", () => {
    renderWindow(true, { interactiveLoginRequired: true });
    expect(screen.getByTestId("login-banner")).toBeInTheDocument();
    expect(screen.queryByTestId("messages")).not.toBeInTheDocument();
  });

  it("shows the login banner when the message page itself needs an interactive login", async () => {
    fetchMessages.mockRejectedValue(
      new ApiError("login", 403, "Forbidden", { code: "interactive_login_required" }),
    );
    renderWindow(true, { seed: () => {} });
    expect(await screen.findByTestId("login-banner")).toBeInTheDocument();
    expect(screen.queryByText("empty")).not.toBeInTheDocument();
  });
});

// The window's annotation hook needs the message ids, but the observer that
// reads them must leave with the fading list (the floating-chat lifetime), so
// the list reports them upward instead of the window subscribing itself.
describe("message report for annotations", () => {
  it("reports the loaded messages while mounted and an empty list once it leaves", async () => {
    const seen: string[][] = [];
    const { setOpen } = renderWindow(true, {
      initial: [{ ...message("a1"), role: "assistant" }],
      onMessages: (messages) => seen.push(messages.map((m) => m.id)),
    });
    await waitFor(() => expect(seen.at(-1)).toEqual(["a1"]));

    setOpen(false);
    await waitFor(() => expect(screen.queryByTestId("messages")).not.toBeInTheDocument());
    await waitFor(() => expect(seen.at(-1)).toEqual([]));
  });
});
