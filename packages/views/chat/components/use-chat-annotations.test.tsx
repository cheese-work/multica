import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { useCommentDraftStore } from "@multica/core/issues/stores";
import type { ChatMessage } from "@multica/core/types";
import type { ReactElement } from "react";
import { renderWithI18n } from "../../test/i18n";
import { ChatAnnotationContext } from "./chat-annotation-context";
import { ChatAnnotationChip } from "./chat-annotations";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));
vi.mock("../../navigation", async (importActual) => ({
  ...(await importActual<typeof import("../../navigation")>()),
  AppLink: ({ href, children }: { href: string; children: React.ReactNode }) => <a href={href}>{children}</a>,
}));
vi.mock("react-virtuoso", () => ({
  Virtuoso: ({ data, itemContent, computeItemKey }: {
    data: unknown[];
    itemContent: (i: number, item: unknown) => ReactElement;
    computeItemKey: (i: number, item: unknown) => string;
  }) => <div>{data.map((item, i) => <div key={computeItemKey(i, item)}>{itemContent(i, item)}</div>)}</div>,
}));

import { ChatMessageList } from "./chat-message-list";
import { useChatAnnotations } from "./use-chat-annotations";

const messages: ChatMessage[] = [
  { id: "u1", chat_session_id: "s1", role: "user", content: "Why does a claim fail?", task_id: null, created_at: "2026-10-20T00:00:00Z" },
  { id: "m1", chat_session_id: "s1", role: "assistant", content: "The delay is fixed at 5 seconds.", task_id: null, created_at: "2026-10-20T00:00:01Z", elapsed_ms: 38000 },
];
const key = "chat:s1" as const;

function Harness({ enabled = true, items = messages }: { enabled?: boolean; items?: ChatMessage[] }) {
  const annotation = useChatAnnotations({ sessionId: "s1", messages: items, agentName: "Terra", enabled });
  const annotations = useCommentDraftStore((s) => s.getAnnotations(key));
  return (
    <div ref={annotation.cardRef} {...annotation.captureProps}>
      {annotation.popup}
      <ChatAnnotationContext.Provider value={annotation.scope}>
        <ChatMessageList messages={items} pendingTask={null} availability="online" sessionId="s1" />
      </ChatAnnotationContext.Provider>
      {annotation.input && annotations.length > 0 && (
        <ChatAnnotationChip draftKey={key} annotations={annotations} disabled={false}
          onEditAnnotation={annotation.input.onEditAnnotation} activeId={annotation.input.activeId}
          onActiveChange={annotation.input.onActiveChange} />
      )}
    </div>
  );
}

function renderHarness(props: { enabled?: boolean; items?: ChatMessage[] } = {}) {
  return renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><Harness {...props} /></WorkspaceSlugProvider></QueryClientProvider>);
}

function selectReply() {
  const region = screen.getByRole("group", { name: "Message from Terra" });
  fireEvent.pointerDown(region, { pointerType: "mouse", button: 0 });
  const range = document.createRange();
  range.selectNodeContents(region.querySelector("p")!);
  act(() => { window.getSelection()!.removeAllRanges(); window.getSelection()!.addRange(range); });
  fireEvent.pointerUp(region);
}

beforeEach(() => {
  useCommentDraftStore.setState({ drafts: {} });
  Object.defineProperty(document.documentElement, "clientWidth", { configurable: true, value: 1024 });
  Object.defineProperty(document.documentElement, "clientHeight", { configurable: true, value: 768 });
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue(new DOMRect(10, 10, 600, 400));
  Range.prototype.getBoundingClientRect = vi.fn(() => new DOMRect(10, 10, 120, 20));
  Range.prototype.getClientRects = vi.fn(() => [] as unknown as DOMRectList);
});

describe("comment on an agent reply in a DM", () => {
  it("selects card 2, retains its request identity, and refuses notes on another source until sent or cleared", async () => {
    const requests = [1, 2].map((number) => ({
      ...messages[1]!, id: `ping-${number}`, message_kind: "decision_request" as const,
      decision_request: { id: `request-${number}`, issue_id: `issue-${number}`, issue_identifier: `CHE-216${number}`, ask: `C. CHE-216${number}`, state: "open" as const, created_at: messages[1]!.created_at },
    }));
    const { container } = renderHarness({ items: [...messages, ...requests] });
    const select = (source: string) => {
      const region = container.querySelector(`[data-comment-content="${source}"]`)!;
      fireEvent.pointerDown(region, { pointerType: "mouse", button: 0 });
      const range = document.createRange();
      range.selectNodeContents(region.querySelector("p")!);
      act(() => { window.getSelection()!.removeAllRanges(); window.getSelection()!.addRange(range); });
      fireEvent.pointerUp(region);
    };
    select("decision-request:request-2");
    fireEvent.click(await screen.findByRole("button", { name: "Add comment" }));
    fireEvent.change(await screen.findByRole("textbox", { name: "Comment 1" }), { target: { value: "1" } });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    expect(useCommentDraftStore.getState().getAnnotations(key)).toMatchObject([
      { sourceCommentId: "decision-request:request-2", quote: "C. CHE-2162", note: "1" },
    ]);
    select("decision-request:request-1");
    expect(screen.queryByRole("button", { name: "Add comment" })).not.toBeInTheDocument();
    select("m1");
    expect(screen.queryByRole("button", { name: "Add comment" })).not.toBeInTheDocument();
    act(() => { useCommentDraftStore.getState().removeAnnotation(key, useCommentDraftStore.getState().getAnnotations(key)[0]!.id); });
    select("decision-request:request-1");
    expect(await screen.findByRole("button", { name: "Add comment" })).toBeInTheDocument();
  });

  it("selects text, adds a comment, and lands it in the DM draft with a marker in reading order", async () => {
    const user = userEvent.setup();
    renderHarness();
    selectReply();
    fireEvent.click(await screen.findByRole("button", { name: "Add comment" }));
    fireEvent.change(await screen.findByRole("textbox", { name: "Comment 1" }), { target: { value: "Why 5 s?" } });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));

    expect(useCommentDraftStore.getState().getAnnotations(key)).toMatchObject([
      { sourceCommentId: "m1", quote: "The delay is fixed at 5 seconds.", note: "Why 5 s?" },
    ]);
    const marker = await screen.findByRole("button", { name: "Edit comment 1: Why 5 s?" });
    await waitFor(() => expect(marker).toHaveFocus());
    expect(screen.getByRole("button", { name: "1 comment" })).toBeInTheDocument();

    // Reading order: the reply, then its marker, then the footer's Copy button.
    screen.getByRole("group", { name: "Message from Terra" }).focus();
    await user.tab();
    expect(marker).toHaveFocus();
    await user.tab();
    expect(screen.getByTestId("assistant-message-copy")).toHaveFocus();
  });

  it("opens the note at the marker from the list's Edit button", async () => {
    useCommentDraftStore.getState().addAnnotation(key, {
      id: "a1", sourceCommentId: "m1", sourceActorName: "Terra", quote: "The delay is fixed at 5 seconds.",
      note: "Why 5 s?", start: 0, prefix: "", suffix: "",
    });
    renderHarness();
    fireEvent.click(await screen.findByRole("button", { name: "1 comment" }));
    fireEvent.click(await screen.findByRole("button", { name: "Edit comment 1" }));
    expect(await screen.findByRole("textbox", { name: "Comment 1" })).toHaveValue("Why 5 s?");
  });

  it("offers no comments while the DM is read-only", () => {
    renderHarness({ enabled: false });
    expect(screen.queryByRole("group", { name: "Message from Terra" })).not.toBeInTheDocument();
  });
});
