import { describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { NavigationProvider } from "../../navigation";
import { chatKeys } from "@multica/core/chat/queries";
import type { Attachment, ChatMessage, ChatSession, TaskMessagePayload } from "@multica/core/types";
import type { ComponentProps, ReactElement } from "react";
import enChat from "../../locales/en/chat.json";
import zhHansAgents from "../../locales/zh-Hans/agents.json";
import zhHansChat from "../../locales/zh-Hans/chat.json";

// The live timeline is a real list row rather than Virtuoso chrome (MUL-4922),
// so it shares one identity with the persisted assistant row and keeps its
// Mermaid/HTML blocks mounted across task completion. Real react-virtuoso
// renders its Footer but NO data rows under jsdom's zero-height viewport, so
// these tests must stub it to see rows at all. computeItemKey is still applied
// here — it is what expresses the live -> persisted identity.
vi.mock("react-virtuoso", () => ({
  Virtuoso: ({
    data,
    itemContent,
    computeItemKey,
    components,
    context,
    followOutput,
    initialTopMostItemIndex,
  }: {
    data: unknown[];
    itemContent: (i: number, item: unknown) => ReactElement;
    computeItemKey: (i: number, item: unknown) => string;
    components?: { Header?: (p: { context?: unknown }) => ReactElement | null; Footer?: (p: { context?: unknown }) => ReactElement | null };
    context?: unknown;
    followOutput?: (atBottom: boolean) => "smooth" | "auto" | false;
    initialTopMostItemIndex?: unknown;
  }) => {
    const Header = components?.Header;
    const Footer = components?.Footer;
    return (
      <div
        data-follow-at-bottom={String(followOutput?.(true))}
        data-follow-away-from-bottom={String(followOutput?.(false))}
        data-initial-position={JSON.stringify(initialTopMostItemIndex)}
      >
        {Header ? <Header context={context} /> : null}
        {data.map((item, i) => (
          <div key={computeItemKey(i, item)} data-row-key={computeItemKey(i, item)}>
            {itemContent(i, item)}
          </div>
        ))}
        {Footer ? <Footer context={context} /> : null}
      </div>
    );
  },
}));

vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => <span data-testid="agent-avatar" /> }));
vi.mock("@multica/core/workspace/hooks", () => ({ useActorName: () => ({ getActorName: () => "Terra" }) }));

import { ChatMessageList } from "./chat-message-list";

const TEST_RESOURCES = { en: { chat: enChat } };
const TASK_ID = "6af44cbe-80ab-4dfe-b07d-bd3cfd588f4d";

describe("ChatMessageList continuation announcements", () => {
  const previousSessionId = "22222222-2222-4222-8222-222222222222";
  const nextSessionId = "33333333-3333-4333-8333-333333333333";
  const summary = {
    id: "summary", chat_session_id: nextSessionId, role: "assistant" as const,
    content: "Retained context without a previous DM link. ".repeat(200),
    message_kind: "continuation_summary" as const, task_id: null, created_at: "2026-10-13T00:00:00Z",
  };

  function renderContinuation(links: { continuedFromSessionId?: string; continuedIntoSessionId?: string } = {}, messages: ChatMessage[] = [summary]) {
    return render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <WorkspaceSlugProvider slug="acme">
          <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (path) => path }}>
            <QueryClientProvider client={new QueryClient()}>
              <ChatMessageList messages={messages} pendingTask={null} availability="online" {...links} />
            </QueryClientProvider>
          </NavigationProvider>
        </WorkspaceSlugProvider>
      </I18nProvider>,
    );
  }

  it("puts the metadata-driven source announcement and a collapsed summary in one block and opens at the newest item", () => {
    const { container } = renderContinuation({ continuedFromSessionId: previousSessionId });
    const link = screen.getByRole("link", { name: "previous DM" });
    expect(link).toHaveAttribute("href", `/acme/chat?session=${previousSessionId}`);
    expect(link.parentElement).toHaveClass("text-muted-foreground");
    expect(link.parentElement).toHaveTextContent("Continued from previous DM");
    const disclosure = screen.getByRole("button", { name: "Show earlier context" });
    expect(screen.queryByRole("button", { name: "Conversation summary" })).not.toBeInTheDocument();
    expect(disclosure).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByText(/Retained context/)).not.toBeInTheDocument();
    expect(container.querySelector("[data-initial-position]")).toHaveAttribute("data-initial-position", JSON.stringify({ index: "LAST", align: "end" }));
    fireEvent.click(disclosure);
    expect(disclosure).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByText(/Retained context/)).toBeInTheDocument();
  });

  it("shows the archived source's successor link at the end of its transcript", () => {
    renderContinuation({ continuedIntoSessionId: nextSessionId });
    const link = screen.getByRole("link", { name: "Continued in →" });
    expect(link).toHaveAttribute("href", `/acme/chat?session=${nextSessionId}`);
    expect(link.parentElement).toHaveClass("text-muted-foreground");
    expect(screen.getByRole("button", { name: "Show earlier context" }).compareDocumentPosition(link) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("keeps legacy summaries readable without inventing a continuation link", () => {
    renderContinuation();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
    expect(screen.getByText("Conversation summary")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Show earlier context" }));
    expect(screen.getByText(/Retained context/)).toBeInTheDocument();
  });

  it("links forward even when the archived source had no messages", () => {
    renderContinuation({ continuedIntoSessionId: nextSessionId }, []);
    expect(screen.getByRole("link", { name: "Continued in →" })).toHaveAttribute("href", `/acme/chat?session=${nextSessionId}`);
  });
});

describe("ChatMessageList predecessor DMs", () => {
  const [first, second, third] = [
    "11111111-1111-4111-8111-111111111111",
    "22222222-2222-4222-8222-222222222222",
    "33333333-3333-4333-8333-333333333333",
  ] as const;
  const session = (id: string, overrides: Partial<ChatSession> = {}): ChatSession => ({
    id, workspace_id: "ws", agent_id: "agent", creator_id: "u", title: `DM ${id.slice(0, 1)}`,
    status: "archived", has_unread: false, created_at: "2026-10-13T00:00:00Z",
    updated_at: "2026-10-13T00:00:00Z", ...overrides,
  });
  const chain = [
    session(first, { continued_into_session_id: second }),
    session(second, { continued_from_session_id: first, continued_into_session_id: third }),
    session(third, { status: "active", continued_from_session_id: second }),
  ];
  const message = (sessionId: string, id: string, role: "user" | "assistant", content: string): ChatMessage => ({
    id, chat_session_id: sessionId, role, content, task_id: null, created_at: "2026-10-12T00:00:00Z",
  });
  const summary: ChatMessage = {
    id: "sum", chat_session_id: third, role: "assistant", content: "Summary of the past",
    message_kind: "continuation_summary", task_id: null, created_at: "2026-10-13T00:00:00Z",
  };

  const expandContext = () => fireEvent.click(screen.getByRole("button", { name: "Show earlier context" }));

  function renderChain({ onQuoteMessage, sessions = chain }: { onQuoteMessage?: (content: string) => void; sessions?: ChatSession[] | null } = {}) {
    const qc = new QueryClient();
    const seed = (id: string, messages: ChatMessage[]) =>
      qc.setQueryData(chatKeys.messagesPage(id), { pages: [{ messages, limit: 50, has_more: false }], pageParams: [null] });
    seed(first, [message(first, "a1", "user", "Oldest question"), message(first, "a2", "assistant", "Oldest answer")]);
    seed(second, [message(second, "b1", "user", "Middle question"), message(second, "b2", "assistant", "Middle **answer**\nsecond line")]);
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <WorkspaceSlugProvider slug="acme">
          <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (path) => path }}>
            <QueryClientProvider client={qc}>
              <ChatMessageList
                messages={[summary]} pendingTask={null} availability="online" sessionId={third}
                continuedFromSessionId={second} sessions={sessions ?? undefined} onQuoteMessage={onQuoteMessage}
              />
            </QueryClientProvider>
          </NavigationProvider>
        </WorkspaceSlugProvider>
      </I18nProvider>,
    );
  }

  it("shows one collapsed section per earlier DM of a 3-DM chain, oldest first", () => {
    renderChain();
    expandContext();
    const sections = screen.getAllByRole("button", { name: /^Earlier DM/ });
    expect(sections.map((b) => b.textContent)).toEqual(["Earlier DM · DM 1", "Earlier DM · DM 2"]);
    for (const section of sections) expect(section).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByText("Oldest question")).not.toBeInTheDocument();
    expect(screen.queryByText("Middle question")).not.toBeInTheDocument();
    // Above the new DM's own messages.
    // The summary shares the one expand with them.
    expect(screen.getByText("Summary of the past")).toBeInTheDocument();
  });

  it("expands a section to its messages, read-only", () => {
    renderChain();
    expandContext();
    fireEvent.click(screen.getByRole("button", { name: "Earlier DM · DM 1" }));
    expect(screen.getByText("Oldest question")).toBeInTheDocument();
    expect(screen.getByText("Oldest answer")).toBeInTheDocument();
    expect(screen.queryByText("Middle question")).not.toBeInTheDocument();
    expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
  });

  it("quotes the exact text of a predecessor message", () => {
    const onQuoteMessage = vi.fn();
    renderChain({ onQuoteMessage });
    expandContext();
    fireEvent.click(screen.getByRole("button", { name: "Earlier DM · DM 2" }));
    const quotes = screen.getAllByRole("button", { name: "Quote" });
    expect(quotes).toHaveLength(2);
    fireEvent.click(quotes[1]!);
    expect(onQuoteMessage).toHaveBeenCalledExactlyOnceWith("Middle **answer**\nsecond line");
  });

  it("offers no quote action when the host cannot take one", () => {
    renderChain();
    expandContext();
    fireEvent.click(screen.getByRole("button", { name: "Earlier DM · DM 2" }));
    expect(screen.getByText("Middle question")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Quote" })).not.toBeInTheDocument();
  });

  it("falls back to the single previous-DM link while the session list is unknown", () => {
    renderChain({ sessions: null });
    expect(screen.queryByRole("button", { name: /^Earlier DM/ })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "previous DM" })).toHaveAttribute("href", `/acme/chat?session=${second}`);
  });

  it("stops at a chain loop instead of repeating sections", () => {
    const loop = [
      session(first, { continued_from_session_id: second }),
      session(second, { continued_from_session_id: first }),
      session(third, { status: "active", continued_from_session_id: second }),
    ];
    renderChain({ sessions: loop });
    expandContext();
    expect(screen.getAllByRole("button", { name: /^Earlier DM/ })).toHaveLength(2);
  });
});

describe("ChatMessageList fork links", () => {
  const sourceId = "44444444-4444-4444-8444-444444444444";
  const childId = "55555555-5555-4555-8555-555555555555";
  const otherChildId = "66666666-6666-4666-8666-666666666666";
  const message = (id: string, role: "user" | "assistant", content: string): ChatMessage => ({
    id, chat_session_id: childId, role, content, task_id: null, created_at: "2026-10-13T00:00:00Z",
  });
  const copiedUser = message("m1", "user", "Copied question");
  const copiedReply = message("m2", "assistant", "Copied answer");
  const newUser = message("m3", "user", "First new message");
  const copied = [copiedUser, copiedReply];
  const session = (overrides: Partial<ChatSession>): ChatSession => ({
    id: sourceId, workspace_id: "ws", agent_id: "agent", creator_id: "u", title: "",
    status: "active", has_unread: false, created_at: "2026-10-13T00:00:00Z",
    updated_at: "2026-10-13T00:00:00Z", ...overrides,
  });
  const forkProps = {
    sessionId: childId,
    forkedFromSessionId: sourceId,
    forkLastMessageId: "m2",
    forkMessageCount: 2,
    sessions: [session({ title: "Launch plan" })],
  };

  function renderFork(props: Partial<ComponentProps<typeof ChatMessageList>>, messages: ChatMessage[] = copied) {
    return render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <WorkspaceSlugProvider slug="acme">
          <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (path) => path }}>
            <QueryClientProvider client={new QueryClient()}>
              <ChatMessageList messages={messages} pendingTask={null} availability="online" {...props} />
            </QueryClientProvider>
          </NavigationProvider>
        </WorkspaceSlugProvider>
      </I18nProvider>,
    );
  }

  it("draws one dimmed divider inside the last copied message's row, below it", () => {
    const { container } = renderFork(forkProps, [...copied, newUser]);
    const separators = screen.getAllByRole("separator");
    expect(separators).toHaveLength(1);
    const divider = separators[0]!;
    const row = divider.closest("[data-row-key]")!;
    expect(row).toHaveTextContent("Copied answer");
    expect(row).not.toHaveTextContent("Copied question");
    expect(row).not.toHaveTextContent("First new message");
    expect(row.querySelectorAll("[role=separator]")).toHaveLength(1);
    expect(container.querySelectorAll("[data-row-key]")).toHaveLength(3);
    expect(screen.getByText("Copied answer").compareDocumentPosition(divider) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(divider.parentElement).toHaveClass("text-muted-foreground");
    expect(divider).toHaveAccessibleName("Forked from Launch plan · 2 messages copied");
  });

  it("links the titled source in the app and states how many messages were copied", () => {
    renderFork(forkProps);
    const link = screen.getByRole("link", { name: "Launch plan" });
    expect(link).toHaveAttribute("href", `/acme/chat?session=${sourceId}`);
    // A separator's children are presentational to assistive tech, so the
    // link must sit beside the separator, not inside it.
    expect(screen.getByRole("separator")).not.toContainElement(link);
    expect(screen.getByRole("separator").parentElement).toHaveTextContent("Forked from Launch plan · 2 messages copied");
  });

  it("falls back to 'original DM' for an untitled source", () => {
    renderFork({ ...forkProps, sessions: [session({ title: "  " })] });
    expect(screen.getByRole("link", { name: "original DM" })).toHaveAttribute("href", `/acme/chat?session=${sourceId}`);
  });

  it("shows plain text, no link, when the source is not in the loaded list", () => {
    renderFork({ ...forkProps, sessions: [session({ id: otherChildId })] });
    expect(screen.getByRole("separator").parentElement).toHaveTextContent("Forked from a deleted DM · 2 messages copied");
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it("does not call the source deleted while the session list is still loading", () => {
    renderFork({ ...forkProps, sessions: undefined });
    expect(screen.queryByText(/deleted DM/)).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "original DM" })).toBeInTheDocument();
  });

  it.each([
    [1, "Forked from Launch plan · 1 message copied"],
    [2, "Forked from Launch plan · 2 messages copied"],
    [120, "Forked from Launch plan · 120 messages copied"],
    [null, "Forked from Launch plan"],
  ])("words a copied count of %s", (count, text) => {
    renderFork({ ...forkProps, forkMessageCount: count });
    expect(screen.getByRole("separator").parentElement).toHaveTextContent(new RegExp(`^${text}$`));
  });

  it("draws no divider until the fork point row is loaded", () => {
    renderFork({ ...forkProps, forkLastMessageId: "m-older", hasOlderMessages: true });
    expect(screen.queryByRole("separator")).not.toBeInTheDocument();
    expect(screen.queryByText(/Forked from/)).not.toBeInTheDocument();
  });

  it("draws no divider on a session that was not forked", () => {
    renderFork({ sessionId: childId });
    expect(screen.queryByRole("separator")).not.toBeInTheDocument();
    expect(screen.queryByText("Send a message to start this branch.")).not.toBeInTheDocument();
  });

  it("shows the start-branch hint until a user message follows the fork point", () => {
    const { unmount } = renderFork(forkProps);
    const hint = screen.getByText("Send a message to start this branch.");
    expect(hint).toHaveClass("text-muted-foreground");
    expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
    unmount();
    renderFork(forkProps, [...copied, newUser]);
    expect(screen.queryByText("Send a message to start this branch.")).not.toBeInTheDocument();
  });

  it("ignores copied user messages and a reply that follows the fork point", () => {
    renderFork(forkProps, [...copied, message("m4", "assistant", "Late reply")]);
    expect(screen.getByText("Send a message to start this branch.")).toBeInTheDocument();
  });

  it("lists no fork links on a source without children", () => {
    renderFork({ sessionId: sourceId, sessions: [session({ title: "Launch plan" })] });
    expect(screen.queryByText(/Forked into/)).not.toBeInTheDocument();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it("links a source to its one child and never says 'Continued in'", () => {
    renderFork({
      sessionId: sourceId,
      sessions: [session({}), session({ id: childId, title: "Branch A", forked_from_session_id: sourceId })],
    });
    const link = screen.getByRole("link", { name: "Branch A" });
    expect(link).toHaveAttribute("href", `/acme/chat?session=${childId}`);
    expect(link.parentElement).toHaveClass("text-muted-foreground");
    expect(link.parentElement).toHaveTextContent("Forked into Branch A");
    expect(screen.queryByText(/Continued in/)).not.toBeInTheDocument();
  });

  it("lists children oldest first with a 'new DM' fallback and ignores other sources' children", () => {
    renderFork({
      sessionId: sourceId,
      sessions: [
        session({}),
        session({ id: otherChildId, title: "", forked_from_session_id: sourceId, created_at: "2026-10-13T02:00:00Z" }),
        session({ id: childId, title: "Branch A", forked_from_session_id: sourceId, created_at: "2026-10-13T01:00:00Z" }),
        session({ id: "77777777-7777-4777-8777-777777777777", title: "Elsewhere", forked_from_session_id: otherChildId }),
      ],
    });
    expect(screen.getAllByRole("link").map((link) => link.textContent)).toEqual(["Branch A", "new DM"]);
  });

  describe("fork-point rows", () => {
    const m = (n: number, createdAt: string) => ({
      ...message(`m${n}`, n % 2 ? "user" : "assistant", `Message ${n}`), created_at: createdAt,
    });
    const five = [1, 2, 3, 4, 5].map((n) => m(n, `2026-10-13T00:0${n}:00Z`));
    const child = (id: string, title: string, extra: Partial<ChatSession>) =>
      session({ id, title, forked_from_session_id: sourceId, ...extra });
    const rowOf = (text: string) => screen.getByText(text).closest("[data-row-key]") as HTMLElement;
    const fork = (overrides: Partial<ChatSession>[]) =>
      renderFork({
        sessionId: sourceId,
        sessions: [session({}), ...overrides.map((o, i) => child(`child-${i}`, `Branch ${i}`, o))],
      }, five);

    it("renders 'Forked into' under the fork-point message, not at the end", () => {
      fork([{ fork_source_message_id: "m2", created_at: "2026-10-13T01:00:00Z" }]);
      const link = screen.getByRole("link", { name: "Branch 0" });
      expect(rowOf("Message 2")).toContainElement(link);
      expect(rowOf("Message 5")).not.toContainElement(link);
      const order = (a: Node, b: Node) => a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING;
      expect(order(link, screen.getByText("Message 3"))).toBeTruthy();
    });

    it("stacks several forks from one message oldest first", () => {
      fork([
        { fork_source_message_id: "m2", created_at: "2026-10-13T02:00:00Z" },
        { fork_source_message_id: "m2", created_at: "2026-10-13T01:00:00Z" },
        { fork_source_message_id: "m4", created_at: "2026-10-13T03:00:00Z" },
      ]);
      expect(within(rowOf("Message 2")).getAllByRole("link").map((l) => l.textContent)).toEqual(["Branch 1", "Branch 0"]);
      expect(within(rowOf("Message 4")).getAllByRole("link").map((l) => l.textContent)).toEqual(["Branch 2"]);
    });

    it("falls back to created_at when the fork point is not loaded, never the footer", () => {
      fork([{ fork_source_message_id: "m-older", created_at: "2026-10-13T00:03:30Z" }]);
      const link = screen.getByRole("link", { name: "Branch 0" });
      expect(rowOf("Message 3")).toContainElement(link);
    });

    it("puts a fork older than the whole loaded page before the first message", () => {
      fork([{ fork_source_message_id: "m-older", created_at: "2026-10-13T00:00:30Z" }]);
      const link = screen.getByRole("link", { name: "Branch 0" });
      expect(rowOf("Message 1")).toContainElement(link);
      expect(link.compareDocumentPosition(screen.getByText("Message 1")) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    });

    it("anchors on the copied source message, not the child's own copy, past a late-visible queued row", () => {
      // m3 was queued when the fork copied m1..m2, so it is not copied and
      // surfaces later with a time before the fork's creation.
      const withQueued = [m(1, "2026-10-13T00:01:00Z"), m(2, "2026-10-13T00:02:00Z"), m(3, "2026-10-13T00:02:30Z")];
      renderFork({
        sessionId: sourceId,
        sessions: [session({}), child("child-0", "Branch 0", {
          fork_source_message_id: "m2", fork_last_message_id: "child-copy-of-m2", created_at: "2026-10-13T00:03:00Z",
        })],
      }, withQueued);
      const link = screen.getByRole("link", { name: "Branch 0" });
      expect(rowOf("Message 2")).toContainElement(link);
      expect(link.compareDocumentPosition(screen.getByText("Message 3")) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    });

    it("keeps 'Continued in' in the footer", () => {
      renderFork({ sessionId: sourceId, continuedIntoSessionId: childId, sessions: [session({})] }, five);
      expect(screen.getByRole("link", { name: /Continued in/ }).closest("[data-row-key]")).toBeNull();
    });
  });
});

function taskMsg(
  seq: number,
  type: TaskMessagePayload["type"],
  extra: Partial<TaskMessagePayload> = {},
): TaskMessagePayload {
  return { task_id: TASK_ID, seq, type, ...extra } as TaskMessagePayload;
}

function pdfAttachment(id: string): Attachment {
  return {
    id,
    workspace_id: "workspace-1",
    issue_id: null,
    comment_id: null,
    chat_session_id: "session-1",
    chat_message_id: null,
    uploader_type: "member",
    uploader_id: "member-1",
    filename: "report.pdf",
    url: "/uploads/report.pdf",
    download_url: `/api/attachments/${id}/download`,
    markdown_url: `/api/attachments/${id}/download`,
    content_type: "application/pdf",
    size_bytes: 1024,
    created_at: "2026-09-17T00:00:00Z",
  };
}

// A streaming timeline whose middle (tool steps) is non-empty, so the live
// footer renders the "N steps" outer fold.
const INITIAL_MESSAGES: TaskMessagePayload[] = [
  taskMsg(0, "text", { content: "Looking into it. " }),
  taskMsg(1, "tool_use", { tool: "Bash", input: { command: "go test ./..." } }),
  taskMsg(2, "tool_result", { tool: "Bash", output: "ok" }),
];

function renderList(qc: QueryClient) {
  qc.setQueryData(chatKeys.taskMessages(TASK_ID), INITIAL_MESSAGES);
  return render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <QueryClientProvider client={qc}>
        <ChatMessageList
          messages={[]}
          pendingTask={{ task_id: TASK_ID, status: "running" }}
          availability={undefined}
        />
      </QueryClientProvider>
    </I18nProvider>,
  );
}

function pushTaskMessage(qc: QueryClient, msg: TaskMessagePayload) {
  // Mirrors useRealtimeSync's task:message handler: a new array lands in the
  // shared task-messages cache on every streamed message.
  act(() => {
    qc.setQueryData<TaskMessagePayload[]>(
      chatKeys.taskMessages(TASK_ID),
      (old = []) => [...old, msg],
    );
  });
}

describe("ChatMessageList live follow (#6697)", () => {
  it("follows appended output immediately only while Virtuoso is at the live end", () => {
    const { container } = render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={new QueryClient()}>
          <ChatMessageList
            messages={[]}
            pendingTask={null}
            availability="online"
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    const list = container.querySelector("[data-follow-at-bottom]");
    expect(list).toHaveAttribute("data-follow-at-bottom", "auto");
    expect(list).toHaveAttribute("data-follow-away-from-bottom", "false");
  });

  it("does not follow while older history is being prepended", () => {
    const { container } = render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={new QueryClient()}>
          <ChatMessageList
            messages={[]}
            pendingTask={null}
            availability="online"
            isFetchingOlderMessages
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    expect(container.querySelector("[data-follow-at-bottom]")).toHaveAttribute(
      "data-follow-at-bottom",
      "false",
    );
  });
});

describe("ChatMessageList live timeline (MUL-3960 regression)", () => {
  // The live footer is passed to Virtuoso through `components`. If that prop
  // is rebuilt inline on render, every streamed task:message unmounts and
  // remounts the whole footer subtree — re-parsing all Markdown and rebuilding
  // thousands of DOM rows, which froze the renderer during long agent runs.
  it("does not remount the live timeline when a streamed message arrives", async () => {
    const qc = new QueryClient();
    renderList(qc);

    const foldTrigger = await screen.findByText("2 steps");
    const footerBefore = foldTrigger.closest("div");

    pushTaskMessage(
      qc,
      taskMsg(3, "tool_use", { tool: "Read", input: { file_path: "/tmp/x" } }),
    );

    // The fold re-renders in place: same DOM node, updated count.
    const updatedTrigger = await screen.findByText("3 steps");
    expect(updatedTrigger.closest("div")).toBe(footerBefore);
    expect(document.contains(foldTrigger)).toBe(true);
  });

  it("keeps the process fold closed by the user across streamed messages", async () => {
    const qc = new QueryClient();
    renderList(qc);

    // Streaming defaults the fold open; the user closes it.
    const foldTrigger = await screen.findByText("2 steps");
    expect(screen.getByText("Bash")).toBeInTheDocument();
    act(() => {
      foldTrigger.click();
    });
    expect(screen.queryByText("Bash")).not.toBeInTheDocument();

    pushTaskMessage(
      qc,
      taskMsg(3, "tool_use", { tool: "Read", input: { file_path: "/tmp/x" } }),
    );

    // Before the fix the footer remounted, useState re-seeded defaultOpen and
    // the fold sprang back open on every streamed message.
    await screen.findByText("3 steps");
    expect(screen.queryByText("Bash")).not.toBeInTheDocument();
  });

  it("applies an embedded surface content transform to streamed text", async () => {
    const qc = new QueryClient();
    qc.setQueryData(chatKeys.taskMessages(TASK_ID), [
      taskMsg(0, "text", {
        content:
          "Draft ready.\n<agent_draft>{\"name\":\"Hidden protocol\"}</agent_draft>",
      }),
    ]);

    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={qc}>
          <ChatMessageList
            messages={[]}
            pendingTask={{ task_id: TASK_ID, status: "running" }}
            availability="online"
            transformContent={(content) =>
              content.replace(
                /<agent_draft>[\s\S]*?<\/agent_draft>/g,
                "",
              )
            }
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    expect(await screen.findByText("Draft ready.")).toBeInTheDocument();
    expect(screen.queryByText(/Hidden protocol/)).not.toBeInTheDocument();
  });

  it("hides a partial quick-actions protocol footer while text streams", async () => {
    const qc = new QueryClient();
    qc.setQueryData(chatKeys.taskMessages(TASK_ID), [
      taskMsg(0, "text", {
        content:
          "Draft ready.\n```quick-actions\n[{\"label\":\"Hidden suggestion\"",
      }),
    ]);

    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={qc}>
          <ChatMessageList
            messages={[]}
            pendingTask={{ task_id: TASK_ID, status: "running" }}
            availability="online"
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    expect(await screen.findByText("Draft ready.")).toBeInTheDocument();
    expect(screen.queryByText(/Hidden suggestion/)).not.toBeInTheDocument();
  });

  it("keeps attachments visible when a settled content transform removes their inline reference", async () => {
    const attachmentId = "11111111-2222-3333-4444-555555555555";
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={new QueryClient()}>
          <ChatMessageList
            messages={[{
              id: "transformed-attachment",
              chat_session_id: "session-1",
              role: "assistant",
              content:
                `<agent_draft>!file[report.pdf](/api/attachments/${attachmentId}/download)</agent_draft>` +
                "Visible answer",
              task_id: null,
              created_at: "2026-09-17T00:00:00Z",
              attachments: [pdfAttachment(attachmentId)],
            }]}
            pendingTask={null}
            availability="online"
            transformContent={(content) =>
              content.replace(/<agent_draft>[\s\S]*<\/agent_draft>/, "")
            }
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    expect(await screen.findByText("Visible answer")).toBeInTheDocument();
    expect(screen.getByText("report.pdf")).toBeInTheDocument();
  });

  it("does not offer Copy for an attachment-only reply", async () => {
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={new QueryClient()}>
          <ChatMessageList
            messages={[{
              id: "attachment-only",
              chat_session_id: "session-1",
              role: "assistant",
              content: "",
              task_id: null,
              created_at: "2026-09-17T00:00:00Z",
              attachments: [
                pdfAttachment("11111111-2222-3333-4444-555555555555"),
              ],
            }]}
            pendingTask={null}
            availability="online"
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    expect(await screen.findByText("report.pdf")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Copy" })).not.toBeInTheDocument();
  });

  it("renders the canonical settled answer while retaining process narration", async () => {
    const qc = new QueryClient();
    qc.setQueryData(chatKeys.taskMessages(TASK_ID), [
      taskMsg(0, "text", { content: "first timeline fragment" }),
      taskMsg(1, "thinking", { content: "checking" }),
      taskMsg(2, "text", { content: "second timeline fragment" }),
    ]);

    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={qc}>
          <ChatMessageList
            messages={[{
              id: "settled-answer",
              chat_session_id: "session-1",
              role: "assistant",
              content: "Complete canonical answer",
              task_id: TASK_ID,
              created_at: "2026-09-17T00:00:00Z",
            }]}
            pendingTask={null}
            availability="online"
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    expect(await screen.findByText("Complete canonical answer")).toBeInTheDocument();
    expect(screen.getAllByText("Complete canonical answer")).toHaveLength(1);
    const foldTrigger = screen.getByText("1 step");
    expect(screen.queryByText("first timeline fragment")).not.toBeInTheDocument();
    expect(screen.queryByText("second timeline fragment")).not.toBeInTheDocument();

    fireEvent.click(foldTrigger);
    expect(screen.getByText("first timeline fragment")).toBeInTheDocument();
    expect(screen.queryByText("second timeline fragment")).not.toBeInTheDocument();
    expect(screen.getAllByText("Complete canonical answer")).toHaveLength(1);
  });

  it("keeps the answer node mounted across the live-to-settled handoff", async () => {
    const qc = new QueryClient();
    qc.setQueryData(chatKeys.taskMessages(TASK_ID), [
      taskMsg(0, "tool_use", { tool: "Read", input: { path: "/tmp/x" } }),
      taskMsg(1, "text", { content: "Intermediate narration" }),
      taskMsg(2, "thinking", { content: "Checking the result" }),
      taskMsg(3, "text", { content: "Stable final answer" }),
    ]);

    const view = (
      messages: Parameters<typeof ChatMessageList>[0]["messages"],
      pendingTask: Parameters<typeof ChatMessageList>[0]["pendingTask"],
    ) => (
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={qc}>
          <ChatMessageList
            messages={messages}
            pendingTask={pendingTask}
            availability="online"
          />
        </QueryClientProvider>
      </I18nProvider>
    );

    const { rerender } = render(
      view([], { task_id: TASK_ID, status: "running" }),
    );
    const answerBefore = await screen.findByText("Stable final answer");
    expect(screen.getByText("3 steps")).toBeInTheDocument();

    rerender(view([{
      id: "persisted-answer",
      chat_session_id: "session-1",
      role: "assistant",
      content: "Stable final answer",
      task_id: TASK_ID,
      created_at: "2026-09-17T00:00:00Z",
    }], null));

    expect(await screen.findByText("Stable final answer")).toBe(answerBefore);
    expect(screen.getByText("3 steps")).toBeInTheDocument();
    expect(screen.queryByText("Intermediate narration")).not.toBeInTheDocument();
  });
});

describe("ChatMessageList tool rows (#8835 regression)", () => {
  // An HTTP-proxy MCP tool (treg's `call`) passes `query` as an object of URL
  // parameters. The row summary returned it as-is, React threw on an object
  // child, and expanding the settled fold took down the whole chat.
  it("expands a settled fold whose tool input carries non-string summary fields", async () => {
    const qc = new QueryClient();
    qc.setQueryData(chatKeys.taskMessages(TASK_ID), [
      taskMsg(0, "tool_use", {
        tool: "mcp__treg__call",
        input: {
          endpoint_id: "zerobounce.people.email.verify",
          query: { email: "a@example.com" },
        },
      }),
      taskMsg(1, "tool_result", {
        tool: "mcp__treg__call",
        output: "{\"status\": 200, \"body\": {\"email_status\": \"Verified\"}}",
      }),
      taskMsg(2, "tool_use", { tool: "Read", input: { path: { dir: "/tmp" } } }),
    ]);

    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={qc}>
          <ChatMessageList
            messages={[{
              id: "settled-answer",
              chat_session_id: "session-1",
              role: "assistant",
              content: "The email is verified.",
              task_id: TASK_ID,
              created_at: "2026-09-25T00:00:00Z",
            }]}
            pendingTask={null}
            availability="online"
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    fireEvent.click(await screen.findByText("3 steps"));

    expect(screen.getByText("mcp__treg__call")).toBeInTheDocument();
    expect(screen.getByText("zerobounce.people.email.verify")).toBeInTheDocument();
    expect(screen.getByText("Read")).toBeInTheDocument();
  });

  it("localizes the multi-file patch summary", async () => {
    const qc = new QueryClient();
    qc.setQueryData(chatKeys.taskMessages(TASK_ID), [
      taskMsg(0, "tool_use", {
        tool: "apply_patch",
        input: {
          changes: [
            { path: "src/a.go", kind: "update", diff: "@@\n+x" },
            { path: "src/b.go", kind: "add", content: "y" },
          ],
        },
      }),
    ]);

    render(
      <I18nProvider
        locale="zh-Hans"
        resources={{ "zh-Hans": { chat: zhHansChat, agents: zhHansAgents } }}
      >
        <QueryClientProvider client={qc}>
          <ChatMessageList
            messages={[]}
            pendingTask={{ task_id: TASK_ID, status: "running" }}
            availability="online"
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    expect(await screen.findByText("src/a.go，另有 1 个文件")).toBeInTheDocument();
  });
});

describe("ChatMessageList footer spacing", () => {
  it("keeps the bottom inset when no task is pending", () => {
    const { container } = render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={new QueryClient()}>
          <ChatMessageList
            messages={[
              {
                id: "assistant-idle",
                chat_session_id: "session-idle",
                role: "assistant",
                content: "Idle reply",
                task_id: null,
                created_at: "2026-08-12T00:00:00Z",
              },
            ]}
            pendingTask={null}
            availability="online"
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    const list = container.querySelector("[data-row-key]")?.parentElement;
    expect(list?.lastElementChild).toHaveClass("pb-4");
    expect(screen.queryByText(/working|queued/i)).not.toBeInTheDocument();
  });
});

describe("ChatMessageList quick actions", () => {
  it("renders up to three suggestions and sends the hidden prompt", async () => {
    const qc = new QueryClient();
    const onQuickAction = vi.fn();
    const quickActions = [
      { label: "Draft the brief", prompt: "Draft the complete launch brief", primary: true },
      { label: "Make a checklist", prompt: "Create a two-week launch checklist" },
      { label: "Define success", prompt: "Define the activation success metric" },
    ];

    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={qc}>
          <ChatMessageList
            messages={[{
              id: "assistant-1",
              chat_session_id: "session-1",
              role: "assistant",
              content: "The plan is ready.",
              task_id: null,
              created_at: "2026-07-22T00:00:00Z",
              quick_actions: quickActions,
            }]}
            pendingTask={null}
            availability="online"
            onQuickAction={onQuickAction}
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    expect(await screen.findByRole("button", { name: "Draft the brief" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Make a checklist" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Define success" })).toBeEnabled();
    fireEvent.click(screen.getByRole("button", { name: "Draft the brief" }));
    expect(onQuickAction).toHaveBeenCalledWith(quickActions[0]);
  });

  it("disables suggestions while another reply is running", async () => {
    const qc = new QueryClient();
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={qc}>
          <ChatMessageList
            messages={[{
              id: "assistant-1",
              chat_session_id: "session-1",
              role: "assistant",
              content: "Ready.",
              task_id: null,
              created_at: "2026-07-22T00:00:00Z",
              quick_actions: [{ label: "Continue", prompt: "Continue" }],
            }]}
            pendingTask={null}
            availability="online"
            onQuickAction={vi.fn()}
            quickActionsDisabled
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    expect(await screen.findByRole("button", { name: "Continue" })).toBeDisabled();
  });
});

describe("ChatMessageList quick actions skeleton", () => {
  const assistantMessage = {
    id: "assistant-1",
    chat_session_id: "session-1",
    role: "assistant" as const,
    content: "The plan is ready.",
    task_id: null,
    created_at: "2026-07-22T00:00:00Z",
  };

  it("renders pill skeletons for the message awaiting its supplement", () => {
    const qc = new QueryClient();
    const { container } = render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={qc}>
          <ChatMessageList
            messages={[assistantMessage]}
            pendingTask={null}
            availability="online"
            onQuickAction={vi.fn()}
            quickActionsPendingMessageId="assistant-1"
          />
        </QueryClientProvider>
      </I18nProvider>,
    );
    expect(container.querySelectorAll(".rounded-full[aria-hidden] , [aria-hidden] .rounded-full").length).toBeGreaterThan(0);
    expect(screen.queryByRole("button", { name: /suggested/i })).toBeNull();
  });

  it("shows no skeleton for other messages or without onQuickAction", () => {
    const qc = new QueryClient();
    const { container } = render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={qc}>
          <ChatMessageList
            messages={[assistantMessage]}
            pendingTask={null}
            availability="online"
            quickActionsPendingMessageId="assistant-1"
          />
        </QueryClientProvider>
      </I18nProvider>,
    );
    expect(container.querySelector("[aria-hidden] .rounded-full")).toBeNull();
  });
});

describe("ChatMessageList onboarding kickoff", () => {
  it("hides the product-authored kickoff while rendering Mika's reply", async () => {
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={new QueryClient()}>
          <ChatMessageList
            messages={[
              {
                id: "kickoff",
                chat_session_id: "s1",
                role: "user",
                content: "INTERNAL ONBOARDING PROMPT",
                task_id: TASK_ID,
                created_at: new Date(0).toISOString(),
                message_kind: "onboarding_kickoff",
              },
              {
                id: "reply",
                chat_session_id: "s1",
                role: "assistant",
                content: "Hi, I'm Mika.",
                task_id: TASK_ID,
                created_at: new Date(1).toISOString(),
              },
            ]}
            pendingTask={undefined}
            availability="online"
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    expect(await screen.findByText("Hi, I'm Mika.")).toBeInTheDocument();
    expect(
      screen.queryByText("INTERNAL ONBOARDING PROMPT"),
    ).not.toBeInTheDocument();
  });
});

describe("ChatMessageList channel quote presentation", () => {
  it("renders channel quote content semantically without exposing protocol metadata", async () => {
    const content =
      "> The image contains a celebration emoji.\n\n" +
      "Can you still see this image?";

    const { container } = render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={new QueryClient()}>
          <ChatMessageList
            messages={[
              {
                id: "channel-user-message",
                chat_session_id: "s1",
                role: "user",
                content,
                task_id: TASK_ID,
                created_at: new Date(0).toISOString(),
              },
            ]}
            pendingTask={undefined}
            availability="online"
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    const quote = await screen.findByText("The image contains a celebration emoji.");
    expect(quote.closest("blockquote")).not.toBeNull();
    expect(screen.getByText("Can you still see this image?")).toBeInTheDocument();
    expect(container).not.toHaveTextContent("quoted_message");
    expect(container).not.toHaveTextContent("private-message-id");
    expect(container).not.toHaveTextContent("private-platform-id");
  });

  it("renders a structured channel quote as a semantic list", async () => {
    const content =
      "> Any heading:\n>\n" +
      "> - Plain text item\n" +
      "> - Another item\n" +
      "> - [Labeled reference](https://example.com/reference)\n\n" +
      "Verify this information";

    const { container } = render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={new QueryClient()}>
          <ChatMessageList
            messages={[
              {
                id: "channel-source-list",
                chat_session_id: "s1",
                role: "user",
                content,
                task_id: TASK_ID,
                created_at: new Date(0).toISOString(),
              },
            ]}
            pendingTask={undefined}
            availability="online"
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    const quote = container.querySelector("blockquote");
    expect(quote).not.toBeNull();
    expect(await screen.findByText("Any heading:")).toBeInTheDocument();
    expect(quote?.querySelectorAll("li")).toHaveLength(3);
    expect(quote?.querySelector('a[href="https://example.com/reference"]')).toHaveTextContent(
      "Labeled reference",
    );
    expect(screen.getByText("Verify this information")).toBeInTheDocument();
  });

  it("keeps text around quoted RichText media and separates current RichText", async () => {
    const content =
      "> Quoted rich text before\n>\n" +
      "> ![Quoted image](https://example.com/quoted.png)\n>\n" +
      "> Quoted rich text after\n\n" +
      "Current rich text\n\n" +
      "![Current image](https://example.com/current.png)";

    const { container } = render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={new QueryClient()}>
          <ChatMessageList
            messages={[
              {
                id: "channel-rich-text",
                chat_session_id: "s1",
                role: "user",
                content,
                task_id: TASK_ID,
                created_at: new Date(0).toISOString(),
              },
            ]}
            pendingTask={undefined}
            availability="online"
          />
        </QueryClientProvider>
      </I18nProvider>,
    );

    const quote = container.querySelector("blockquote");
    expect(quote).not.toBeNull();
    expect(quote).toHaveTextContent("Quoted rich text before");
    expect(quote).toHaveTextContent("Quoted rich text after");
    expect(quote?.querySelector('img[alt="Quoted image"]')).not.toBeNull();
    expect(quote?.querySelector('img[alt="Current image"]')).toBeNull();
    expect(screen.getByText("Current rich text")).toBeInTheDocument();
    expect(container.querySelector('img[alt="Current image"]')).not.toBeNull();
  });
});

describe("ChatMessageList failure copy (MUL-5370 regression)", () => {
  // The backend moved to the refined taxonomy (agent_error.*) in MUL-2946 but
  // the copy map stayed on the six coarse values, so an exact-key lookup
  // missed every refined reason and fell through to the generic fallback.
  // A user whose skill bundle download stalled was told only "Something went
  // wrong", and the maintainer debugging it had nothing better to go on.
  function renderFailure(reason: string) {
    return render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={new QueryClient()}>
          <ChatMessageList
            messages={[
              {
                id: "m1",
                chat_session_id: "s1",
                role: "assistant",
                content: "skill bundle unavailable: skill \"x\"",
                task_id: null,
                created_at: new Date(0).toISOString(),
                failure_reason: reason,
              },
            ]}
            pendingTask={undefined}
            availability="online"
          />
        </QueryClientProvider>
      </I18nProvider>,
    );
  }

  const FALLBACK = enChat.message_list.failure.fallback;

  it("explains revoked access with a rebind action rather than a generic retry", async () => {
    renderFailure("runtime_access_revoked");
    expect(await screen.findByText(enChat.message_list.failure.runtime_access_revoked)).toBeInTheDocument();
    expect(screen.queryByText(FALLBACK)).not.toBeInTheDocument();
  });

  it("renders dedicated copy for a stalled skill bundle download", async () => {
    renderFailure("skill_bundle_unavailable");
    expect(
      await screen.findByText(enChat.message_list.failure.skill_bundle_unavailable),
    ).toBeInTheDocument();
    expect(screen.queryByText(FALLBACK)).not.toBeInTheDocument();
  });

  it("renders dedicated copy for a failed environment preparation", async () => {
    // #7913. Without an entry of its own this reason has no agent_error
    // family to degrade into, so it would land on the generic fallback —
    // and the one thing the reader needs to know is that the problem is on
    // the machine running the agent, which the fallback cannot say.
    renderFailure("environment_prepare_failed");
    expect(
      await screen.findByText(
        enChat.message_list.failure.environment_prepare_failed,
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText(FALLBACK)).not.toBeInTheDocument();
  });

  it("renders dedicated recovery copy for persisted runtime access denial", async () => {
    renderFailure("runtime_access_denied");
    expect(
      await screen.findByText(enChat.message_list.failure.runtime_access_denied),
    ).toBeInTheDocument();
    expect(screen.queryByText(FALLBACK)).not.toBeInTheDocument();
  });

  it("renders dedicated copy for a refined reason the map names", async () => {
    renderFailure("agent_error.provider_network");
    expect(
      await screen.findByText(enChat.message_list.failure.provider_network),
    ).toBeInTheDocument();
  });

  it("degrades an unnamed refined reason to its agent_error family", async () => {
    renderFailure("agent_error.unknown");
    expect(
      await screen.findByText(enChat.message_list.failure.agent_error),
    ).toBeInTheDocument();
    expect(screen.queryByText(FALLBACK)).not.toBeInTheDocument();
  });

  it("degrades a reason newer than this build to its agent_error family", async () => {
    renderFailure("agent_error.some_future_bucket");
    expect(
      await screen.findByText(enChat.message_list.failure.agent_error),
    ).toBeInTheDocument();
  });

  it("still falls back when neither the reason nor its family is known", async () => {
    renderFailure("something_entirely_new");
    expect(await screen.findByText(FALLBACK)).toBeInTheDocument();
  });
});

describe("ChatMessageList onboarding starter cards", () => {
  // The opening self-describes: the completion path stamps Mika's reply to the
  // hidden kickoff with message_kind "onboarding_opening" (the kickoff row
  // itself never reaches clients).
  const opening = {
    id: "opening",
    chat_session_id: "s1",
    role: "assistant" as const,
    content: "Hi, I'm Mika.",
    task_id: null,
    created_at: new Date(1).toISOString(),
    message_kind: "onboarding_opening" as const,
    quick_actions: [{ label: "LLM chip", prompt: "llm prompt" }],
  };

  function renderCards(overrides: Partial<Parameters<typeof ChatMessageList>[0]> = {}) {
    return render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={new QueryClient()}>
          <ChatMessageList
            messages={[opening]}
            pendingTask={null}
            availability="online"
            onQuickAction={vi.fn()}
            {...overrides}
          />
        </QueryClientProvider>
      </I18nProvider>,
    );
  }

  it("renders the three cards under the opening and hides that turn's chips", async () => {
    renderCards();
    expect(
      await screen.findByRole("button", { name: "Get a board up in minutes" }),
    ).toBeEnabled();
    expect(screen.getByRole("button", { name: "Hand me one thing first" })).toBeEnabled();
    expect(
      screen.getByRole("button", { name: "Let the daily digest write itself" }),
    ).toBeEnabled();
    // The cards own the opening's suggestion strip — no chip row beside them.
    expect(screen.queryByRole("button", { name: "LLM chip" })).toBeNull();
    expect(screen.queryByText("Follow-up questions")).toBeNull();
  });

  it("sends the card's fixed prompt through the quick-action path", async () => {
    const onQuickAction = vi.fn();
    renderCards({ onQuickAction });
    fireEvent.click(
      await screen.findByRole("button", { name: "Get a board up in minutes" }),
    );
    expect(onQuickAction).toHaveBeenCalledWith({
      label: "Get a board up in minutes",
      prompt: "Turn our current goals into a project board",
    });
  });

  it("follows the chips' disabled rule while a task runs", async () => {
    renderCards({ quickActionsDisabled: true });
    expect(
      await screen.findByRole("button", { name: "Get a board up in minutes" }),
    ).toBeDisabled();
  });

  it("renders ordinary chips, not cards, for an unstamped assistant turn", async () => {
    renderCards({ messages: [{ ...opening, message_kind: "message" as const }] });
    expect(await screen.findByRole("button", { name: "LLM chip" })).toBeEnabled();
    expect(
      screen.queryByRole("button", { name: "Get a board up in minutes" }),
    ).toBeNull();
  });

  it("attaches cards only to the stamped opening; later turns keep chips", async () => {
    const followUp = {
      id: "follow-up",
      chat_session_id: "s1",
      role: "assistant" as const,
      content: "Anything else?",
      task_id: null,
      created_at: new Date(2).toISOString(),
      quick_actions: [{ label: "Later chip", prompt: "later" }],
    };
    renderCards({ messages: [opening, followUp] });
    expect(
      await screen.findByRole("button", { name: "Get a board up in minutes" }),
    ).toBeEnabled();
    expect(screen.getByRole("button", { name: "Later chip" })).toBeEnabled();
  });
});

// CHE-1807: the server-authored reason for an automatic compaction renders as a
// muted note, never as an assistant reply.
describe("ChatMessageList compact notice", () => {
  const notice = {
    id: "notice", chat_session_id: "44444444-4444-4444-8444-444444444444", role: "assistant" as const,
    content: "Auto-compacted: context 164k of 200k tokens (82%) reached the 80% threshold. Continuing in a new DM.",
    message_kind: "compact_notice", task_id: null, created_at: "2026-10-16T00:00:00Z",
  } as unknown as ChatMessage;

  it("shows the reason as a muted note", () => {
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <WorkspaceSlugProvider slug="acme">
          <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (path) => path }}>
            <QueryClientProvider client={new QueryClient()}>
              <ChatMessageList messages={[notice]} pendingTask={null} availability="online" />
            </QueryClientProvider>
          </NavigationProvider>
        </WorkspaceSlugProvider>
      </I18nProvider>,
    );

    const note = screen.getByRole("note");
    expect(note).toHaveTextContent("Auto-compacted: context 164k of 200k tokens (82%) reached the 80% threshold. Continuing in a new DM.");
    expect(note).toHaveClass("text-muted-foreground");
  });
});

describe("ChatMessageList message timestamps (CHE-1871)", () => {
  const at = (iso: string) => new Date(iso);
  const clock = (iso: string) => new Intl.DateTimeFormat(undefined, { timeStyle: "short" }).format(at(iso));
  const msg = (id: string, role: "user" | "assistant", created_at: string): ChatMessage => ({
    id, chat_session_id: "s1", role, content: `body ${id}`, task_id: null, created_at,
  });

  function renderStamped(messages: ChatMessage[]) {
    return render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={new QueryClient()}>
          <ChatMessageList messages={messages} pendingTask={undefined} availability="online" />
        </QueryClientProvider>
      </I18nProvider>,
    );
  }

  it("shows each user and assistant bubble's send time in the client locale", () => {
    const { container } = renderStamped([
      msg("u1", "user", "2026-10-20T04:20:00Z"),
      msg("a1", "assistant", "2026-10-20T04:21:00Z"),
    ]);
    const times = Array.from(container.querySelectorAll("time[datetime]"));
    const bubbleTimes = times.filter((el) => el.hasAttribute("data-message-time"));
    expect(bubbleTimes.map((el) => el.textContent)).toEqual([clock("2026-10-20T04:20:00Z"), clock("2026-10-20T04:21:00Z")]);
    expect(bubbleTimes[0]).toHaveAttribute("datetime", "2026-10-20T04:20:00Z");
  });

  it("puts a day separator between messages from different days only", () => {
    const { container } = renderStamped([
      msg("u1", "user", "2020-01-01T04:20:00Z"),
      msg("u2", "user", "2020-01-01T04:25:00Z"),
      msg("u3", "user", "2020-01-02T04:20:00Z"),
    ]);
    expect(container.querySelectorAll("[data-day-separator]")).toHaveLength(2);
  });
});
