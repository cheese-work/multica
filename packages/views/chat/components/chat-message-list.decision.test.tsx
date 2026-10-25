import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import type { ChatMessage, DecisionCard } from "@multica/core/types";
import { forwardRef, useImperativeHandle, type ReactElement } from "react";
import { NavigationProvider } from "../../navigation";
import { RESOURCES } from "../../locales";

// Real react-virtuoso renders no data rows under jsdom, so it is stubbed. The
// stub also records `scrollToIndex`, which is how a jump to a message lands.
const scrollToIndex = vi.fn();
vi.mock("react-virtuoso", () => ({
  Virtuoso: forwardRef(function VirtuosoStub(
    { data, itemContent, computeItemKey }: {
      data: unknown[];
      itemContent: (i: number, item: unknown) => ReactElement;
      computeItemKey: (i: number, item: unknown) => string;
    },
    ref,
  ) {
    useImperativeHandle(ref, () => ({ scrollToIndex }));
    return (
      <div>
        {data.map((item, i) => (
          <div key={computeItemKey(i, item)} data-row-key={computeItemKey(i, item)}>
            {itemContent(i, item)}
          </div>
        ))}
      </div>
    );
  }),
}));
vi.mock("../../navigation", async (importActual) => ({
  ...(await importActual<typeof import("../../navigation")>()),
  AppLink: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => <a href={href} {...rest}>{children}</a>,
}));

import { ChatMessageList } from "./chat-message-list";

const card = (overrides: Partial<DecisionCard> = {}): DecisionCard => ({
  answer_id: "answer-1",
  issue_id: "issue-1",
  issue_identifier: "CHE-1870",
  issue_comment_id: "comment-1",
  class: "reversible",
  state: "effective",
  effective_at: "2026-10-23T05:00:00Z",
  inherited: false,
  ...overrides,
});

const message = (id: string, extra: Partial<ChatMessage> = {}): ChatMessage => ({
  id,
  chat_session_id: "s1",
  role: "user",
  content: `text ${id}`,
  task_id: null,
  created_at: `2026-10-23T05:0${id.length}:00Z`,
  ...extra,
});

function renderList(props: Partial<React.ComponentProps<typeof ChatMessageList>> & { messages: ChatMessage[] }) {
  const result = render(
    <I18nProvider locale="en" resources={RESOURCES}>
      <WorkspaceSlugProvider slug="acme">
        <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (p) => p }}>
          <QueryClientProvider client={new QueryClient()}>
            <ChatMessageList pendingTask={null} availability="online" sessionId="s1" {...props} />
          </QueryClientProvider>
        </NavigationProvider>
      </WorkspaceSlugProvider>
    </I18nProvider>,
  );
  return {
    ...result,
    rerenderList: (next: Partial<React.ComponentProps<typeof ChatMessageList>> & { messages: ChatMessage[] }) =>
      result.rerender(
        <I18nProvider locale="en" resources={RESOURCES}>
          <WorkspaceSlugProvider slug="acme">
            <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (p) => p }}>
              <QueryClientProvider client={new QueryClient()}>
                <ChatMessageList pendingTask={null} availability="online" sessionId="s1" {...next} />
              </QueryClientProvider>
            </NavigationProvider>
          </WorkspaceSlugProvider>
        </I18nProvider>,
      ),
  };
}

beforeEach(() => scrollToIndex.mockReset());
afterEach(() => vi.useRealTimers());

describe("ChatMessageList — decision cards", () => {
  it("puts the card under the quoted bubble and none under other bubbles", () => {
    renderList({ messages: [message("a"), message("bb", { decision_quotes: [card()] })] });
    const cards = screen.getAllByTestId("decision-dm-card");
    expect(cards).toHaveLength(1);
    const row = cards[0]!.closest("[data-row-key]")!;
    expect(row).toHaveTextContent("text bb");
  });

  it("stacks several quotes of one bubble", () => {
    renderList({ messages: [message("a", { decision_quotes: [card(), card({ answer_id: "answer-2", issue_comment_id: "comment-2" })] })] });
    expect(screen.getAllByTestId("decision-dm-card")).toHaveLength(2);
  });

  it("hides the plain-text 'Decision recorded' message when its card renders, and keeps one without a card", () => {
    renderList({
      messages: [
        message("a", { decision_quotes: [card()] }),
        message("ref1", { role: "assistant", message_kind: "decision_reference", content: "Decision recorded for CHE-1870.", decision_ref: card() }),
        message("ref2", { role: "assistant", message_kind: "decision_reference", content: "Legacy recorded note." }),
      ],
    });
    expect(screen.queryByText("Decision recorded for CHE-1870.")).toBeNull();
    expect(screen.getByText("Legacy recorded note.")).toBeTruthy();
  });
});

describe("ChatMessageList — jump to a message", () => {
  it("centres the exact bubble, flashes it once, then clears the flash", () => {
    vi.useFakeTimers();
    const settled = vi.fn();
    renderList({ messages: [message("a"), message("bb"), message("ccc")], jumpToMessageId: "bb", onJumpSettled: settled });
    expect(scrollToIndex).toHaveBeenCalledWith(expect.objectContaining({ index: 1, align: "center" }));
    expect(settled).toHaveBeenCalledWith("landed");
    const flashed = document.querySelector("[data-flash]");
    expect(flashed).not.toBeNull();
    expect(flashed).toHaveTextContent("text bb");
    act(() => void vi.advanceTimersByTime(1600));
    expect(document.querySelector("[data-flash]")).toBeNull();
  });

  it("keeps re-aiming while the target is outside the viewport, and stops once the reader scrolls", () => {
    vi.useFakeTimers();
    renderList({ messages: [message("a"), message("bb"), message("ccc")], jumpToMessageId: "bb" });
    const landed = scrollToIndex.mock.calls.length;
    act(() => void vi.advanceTimersByTime(200));
    expect(scrollToIndex.mock.calls.length).toBeGreaterThan(landed);
    expect(scrollToIndex).toHaveBeenLastCalledWith(expect.objectContaining({ index: 1, align: "center" }));
    act(() => void document.querySelector("[data-tab-scroll-root]")!.dispatchEvent(new Event("wheel")));
    const afterWheel = scrollToIndex.mock.calls.length;
    act(() => void vi.advanceTimersByTime(200));
    expect(scrollToIndex.mock.calls.length).toBe(afterWheel);
  });

  it("asks for older messages while the target is not loaded, then lands once it is", () => {
    const load = vi.fn();
    const settled = vi.fn();
    const base = { hasOlderMessages: true, onLoadOlderMessages: load, jumpToMessageId: "old", onJumpSettled: settled };
    const { rerenderList } = renderList({ messages: [message("ccc")], ...base });
    expect(load).toHaveBeenCalled();
    expect(scrollToIndex).not.toHaveBeenCalled();
    expect(settled).not.toHaveBeenCalled();
    rerenderList({ messages: [message("old"), message("ccc")], ...base });
    expect(scrollToIndex).toHaveBeenCalledWith(expect.objectContaining({ index: 0, align: "center" }));
    expect(settled).toHaveBeenCalledWith("landed");
  });

  it("reports a missing message when nothing older is left", () => {
    const settled = vi.fn();
    renderList({ messages: [message("a")], hasOlderMessages: false, jumpToMessageId: "gone", onJumpSettled: settled });
    expect(settled).toHaveBeenCalledWith("missing");
    expect(scrollToIndex).not.toHaveBeenCalled();
    expect(document.querySelector("[data-flash]")).toBeNull();
  });

  it("does nothing without a jump target", () => {
    renderList({ messages: [message("a")] });
    expect(scrollToIndex).not.toHaveBeenCalled();
  });
});
