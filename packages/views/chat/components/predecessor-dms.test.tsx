import { act, fireEvent, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useCommentDraftStore } from "@multica/core/issues/stores";
import { chatKeys } from "@multica/core/chat/queries";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import type { ChatMessage, ChatSession } from "@multica/core/types";
import type { ReactElement } from "react";
import { renderWithI18n } from "../../test/i18n";
import { NavigationProvider } from "../../navigation";
import { ChatAnnotationContext } from "./chat-annotation-context";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));
vi.mock("react-virtuoso", () => ({
  Virtuoso: ({ data, itemContent, computeItemKey, components, context }: {
    data: unknown[];
    itemContent: (i: number, item: unknown) => ReactElement;
    computeItemKey: (i: number, item: unknown) => string;
    components?: { Header?: (p: { context?: unknown }) => ReactElement | null };
    context?: unknown;
  }) => {
    const Header = components?.Header;
    return (
      <div>
        {Header ? <Header context={context} /> : null}
        {data.map((item, i) => <div key={computeItemKey(i, item)}>{itemContent(i, item)}</div>)}
      </div>
    );
  },
}));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => <span data-testid="agent-avatar" /> }));
vi.mock("@multica/core/workspace/hooks", () => ({ useActorName: () => ({ getActorName: () => "Terra" }) }));

import { ChatMessageList } from "./chat-message-list";
import { useChatAnnotations } from "./use-chat-annotations";

const [first, second] = ["11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"] as const;
const current = "33333333-3333-4333-8333-333333333333";
const session = (id: string, overrides: Partial<ChatSession> = {}): ChatSession => ({
  id, workspace_id: "ws", agent_id: "agent", creator_id: "u", title: `DM ${id.slice(0, 1)}`,
  status: "archived", has_unread: false, created_at: "2026-10-13T00:00:00Z", updated_at: "2026-10-13T00:00:00Z", ...overrides,
});
const sessions = [
  session(first, { continued_into_session_id: second, last_message: { content: "Oldest answer", role: "assistant", created_at: "2026-10-12T00:00:00Z" } }),
  session(second, { continued_from_session_id: first, continued_into_session_id: current, last_message: { content: "Middle question", role: "user", created_at: "2026-10-12T00:00:00Z" } }),
  session(current, { status: "active", continued_from_session_id: second }),
];
const message = (sessionId: string, id: string, role: "user" | "assistant", content: string): ChatMessage => ({
  id, chat_session_id: sessionId, role, content, task_id: null, created_at: "2026-10-12T00:00:00Z",
});
const summary: ChatMessage = {
  id: "sum", chat_session_id: current, role: "assistant", content: "Summary of the past",
  message_kind: "continuation_summary", task_id: null, created_at: "2026-10-13T00:00:00Z",
};
const key = `chat:${current}` as const;

function Harness({ qc }: { qc: QueryClient }) {
  const messages = [summary];
  const annotation = useChatAnnotations({ sessionId: current, messages, agentName: "Terra", enabled: true });
  return (
    <div ref={annotation.cardRef} {...annotation.captureProps}>
      {annotation.popup}
      <ChatAnnotationContext.Provider value={annotation.scope}>
        <WorkspaceSlugProvider slug="acme">
          <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (path) => path }}>
            <QueryClientProvider client={qc}>
              <ChatMessageList messages={messages} pendingTask={null} availability="online" sessionId={current}
                continuedFromSessionId={second} sessions={sessions} />
            </QueryClientProvider>
          </NavigationProvider>
        </WorkspaceSlugProvider>
      </ChatAnnotationContext.Provider>
    </div>
  );
}

function renderChain() {
  const qc = new QueryClient();
  const seed = (id: string, messages: ChatMessage[]) =>
    qc.setQueryData(chatKeys.messagesPage(id), { pages: [{ messages, limit: 50, has_more: false }], pageParams: [null] });
  seed(first, [message(first, "a1", "user", "Oldest question"), message(first, "a2", "assistant", "Oldest answer")]);
  seed(second, [message(second, "b1", "user", "Middle question"), message(second, "b2", "assistant", "Middle answer")]);
  return renderWithI18n(<Harness qc={qc} />);
}

function select(text: string) {
  const target = screen.getByText(text);
  const host = target.closest("[data-comment-content]") as HTMLElement;
  fireEvent.pointerDown(host, { pointerType: "mouse", button: 0 });
  const range = document.createRange();
  range.selectNodeContents(target);
  act(() => { window.getSelection()!.removeAllRanges(); window.getSelection()!.addRange(range); });
  fireEvent.pointerUp(host);
}

async function addComment(text: string, note: string) {
  select(text);
  fireEvent.click(await screen.findByRole("button", { name: "Add comment" }));
  fireEvent.change(await screen.findByRole("textbox", { name: "Comment 1" }), { target: { value: note } });
  fireEvent.click(screen.getByRole("button", { name: "Add" }));
}

beforeEach(() => {
  useCommentDraftStore.setState({ drafts: {} });
  Object.defineProperty(document.documentElement, "clientWidth", { configurable: true, value: 1024 });
  Object.defineProperty(document.documentElement, "clientHeight", { configurable: true, value: 768 });
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue(new DOMRect(10, 10, 600, 400));
  Range.prototype.getBoundingClientRect = vi.fn(() => new DOMRect(10, 10, 120, 20));
  Range.prototype.getClientRects = vi.fn(() => [] as unknown as DOMRectList);
});

describe("Add comment in the earlier DM blocks", () => {
  it("anchors a comment on an earlier DM agent message to that message", async () => {
    renderChain();
    fireEvent.click(screen.getByRole("button", { name: "Earlier DM · DM 2" }));
    await addComment("Middle answer", "Why?");
    expect(useCommentDraftStore.getState().getAnnotations(key)).toMatchObject([
      { sourceCommentId: "b2", sourceActorName: "Terra", quote: "Middle answer", note: "Why?" },
    ]);
  });

  it("anchors a comment on the Conversation summary to the summary message", async () => {
    renderChain();
    fireEvent.click(screen.getByRole("button", { name: "Conversation summary" }));
    await addComment("Summary of the past", "Is this complete?");
    expect(useCommentDraftStore.getState().getAnnotations(key)).toMatchObject([
      { sourceCommentId: "sum", quote: "Summary of the past", note: "Is this complete?" },
    ]);
  });
});

describe("Speakers in the earlier DM block", () => {
  it("renders a human row as a bubble and an agent row with avatar and name", () => {
    renderChain();
    fireEvent.click(screen.getByRole("button", { name: "Earlier DM · DM 2" }));
    const human = document.querySelector<HTMLElement>("[data-predecessor-message='b1']")!;
    const agent = document.querySelector<HTMLElement>("[data-predecessor-message='b2']")!;
    expect(human).toHaveAttribute("data-speaker", "user");
    expect(agent).toHaveAttribute("data-speaker", "assistant");
    expect(within(human).queryByTestId("agent-avatar")).not.toBeInTheDocument();
    expect(within(human).getByText("Middle question").closest("[data-testid='chat-user-message']")).not.toBeNull();
    expect(within(agent).getByTestId("agent-avatar")).toBeInTheDocument();
    expect(within(agent).getByText("Terra")).toBeInTheDocument();
  });

  it("shows who spoke last in the collapsed preview", () => {
    renderChain();
    const previews = screen.getAllByTestId("predecessor-preview");
    expect(previews[0]).toHaveTextContent("Terra: Oldest answer");
    expect(previews[1]).toHaveTextContent("You: Middle question");
  });
});
