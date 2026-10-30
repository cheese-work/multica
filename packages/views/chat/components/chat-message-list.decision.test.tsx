import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, within } from "@testing-library/react";
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
  const requestCard = {
    id: "request-1",
    issue_id: "issue-1",
    issue_identifier: "CHE-2088",
    source_comment_id: "source-1",
    ask: "Ship the reversible change?",
    state: "open" as const,
    created_at: "2026-10-26T05:00:00Z",
  };

  it("renders an open ask as a labelled card with source issue and comment links", () => {
    renderList({ messages: [{ ...message("ask", { role: "assistant", message_kind: "decision_request" }), decision_request: requestCard }] });
    const region = screen.getByRole("region", { name: "Decision request: Open" });
    expect(region).toHaveTextContent(requestCard.ask);
    expect(screen.getByRole("link", { name: "CHE-2088" })).toHaveAttribute("href", "/acme/issues/issue-1");
    expect(screen.getByRole("link", { name: "Source comment" })).toHaveAttribute("href", "/acme/issues/issue-1#comment-source-1");
    expect(region).toHaveTextContent("12:00 ICT");
  });

  it("keeps the original request row when a cancellation notice references the same request", () => {
    const withdrawn = { ...requestCard, state: "withdrawn" as const, reason: "Superseded by the new scope." };
    const { container } = renderList({ messages: [
      { ...message("ask", { role: "assistant", message_kind: "decision_request" }), decision_request: withdrawn },
      { ...message("notice", { role: "assistant", message_kind: "decision_cancel_notice" }), decision_requests: [withdrawn] },
    ] });
    expect(container.querySelectorAll("[data-row-key]")).toHaveLength(2);
    expect(container.querySelector('[data-row-key="ask"]')).toHaveTextContent(requestCard.ask);
    expect(container.querySelector('[data-row-key="notice"]')).toBeInTheDocument();
  });

  it("pairs the full withdrawn request with a compact announcement without repeating the ask", () => {
    renderList({ messages: [
      { ...message("ask", { role: "assistant", message_kind: "decision_request" }), decision_request: { ...requestCard, state: "withdrawn", reason: "Superseded by the new scope.", withdrawn_at: "2026-10-26T06:30:00Z" } },
      { ...message("notice", { role: "assistant", message_kind: "decision_cancel_notice", content: "Obsolete text announcement" }), decision_requests: [{ ...requestCard, state: "withdrawn", reason: "Superseded by the new scope.", withdrawn_at: "2026-10-26T06:30:00Z" }] },
    ] });
    const region = screen.getByRole("region", { name: "Decision request: Withdrawn" });
    expect(region).toHaveTextContent(requestCard.ask);
    expect(region).toHaveTextContent("Superseded by the new scope.");
    expect(region).toHaveTextContent("13:30 ICT");
    const announcement = screen.getByRole("region", { name: "Withdrawn in CHE-2088" });
    expect(announcement).toHaveTextContent("Withdrawn");
    expect(announcement).toHaveTextContent("Superseded by the new scope.");
    expect(announcement).not.toHaveTextContent(requestCard.ask);
    expect(screen.getAllByText(requestCard.ask)).toHaveLength(1);
    expect(screen.getByRole("link", { name: "Withdrawn in CHE-2088" })).toHaveAttribute("href", "/acme/issues/issue-1#comment-source-1");
    expect(screen.queryByRole("button", { name: "Disclaim" })).toBeNull();
    expect(screen.queryByText("Obsolete text announcement")).toBeNull();
    expect(screen.queryByText(/ask text unavailable/i)).toBeNull();
  });

  it("labels answered asks and leaves legacy asks without source metadata usable", () => {
    renderList({ messages: [{ ...message("ask", { role: "assistant", message_kind: "decision_request" }), decision_request: { ...requestCard, state: "answered", source_comment_id: undefined } }] });
    expect(screen.getByRole("region", { name: "Decision request: Recorded" })).toHaveTextContent(requestCard.ask);
    expect(screen.getByRole("link", { name: "CHE-2088" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Source comment" })).toBeNull();
  });

  it("keeps the text of asks from older servers without any card metadata", () => {
    renderList({ messages: [message("legacy", { role: "assistant", message_kind: "decision_request", content: "Original legacy ask" })] });
    expect(screen.getByText("Original legacy ask")).toBeInTheDocument();
    expect(screen.queryByTestId("decision-request-card")).toBeNull();
  });

  it("renders the compact notice when the original ask is outside the loaded page", () => {
    renderList({ messages: [{ ...message("notice", { role: "assistant", message_kind: "decision_cancel_notice", content: "Legacy cancellation text" }), decision_requests: [{ ...requestCard, state: "withdrawn", reason: "Scope changed." }] }] });
    expect(screen.getByRole("region", { name: "Withdrawn in CHE-2088" })).toBeInTheDocument();
    expect(screen.queryByText(requestCard.ask)).toBeNull();
    expect(screen.queryByText("Legacy cancellation text")).toBeNull();
  });

  it("renders one compact row per grouped withdrawal and falls back to the issue without a source comment", () => {
    renderList({ messages: [{ ...message("notice", { role: "assistant", message_kind: "decision_cancel_notice" }), decision_requests: [
      { ...requestCard, state: "withdrawn", reason: "Issue closed." },
      { ...requestCard, id: "request-2", issue_id: "issue-2", issue_identifier: "", source_comment_id: undefined, state: "withdrawn", ask: "Second ask", reason: "Scope changed." },
    ] }] });
    expect(screen.getAllByTestId("decision-withdrawal-card")).toHaveLength(2);
    expect(screen.getByRole("region", { name: "Withdrawn in CHE-2088" })).toHaveTextContent("Issue closed.");
    expect(screen.getByRole("region", { name: "Withdrawn in issue-2" })).toHaveTextContent("Scope changed.");
    expect(screen.getByRole("link", { name: "Withdrawn in issue-2" })).toHaveAttribute("href", "/acme/issues/issue-2");
    expect(screen.queryByTestId("decision-request-card")).toBeNull();
    expect(screen.queryByText("Second ask")).toBeNull();
  });

  it("keeps both cards when paging loads the original request after its withdrawal notice", () => {
    const withdrawn = { ...requestCard, state: "withdrawn" as const };
    const notice = { ...message("notice", { role: "assistant", message_kind: "decision_cancel_notice" }), decision_requests: [withdrawn] };
    const { rerenderList } = renderList({ messages: [notice] });
    expect(screen.getByTestId("decision-withdrawal-card")).toBeInTheDocument();
    rerenderList({ messages: [{ ...message("ask", { role: "assistant", message_kind: "decision_request" }), decision_request: withdrawn }, notice] });
    expect(screen.getAllByTestId("decision-request-card")).toHaveLength(1);
    expect(screen.getAllByTestId("decision-withdrawal-card")).toHaveLength(1);
  });

  it("keeps a cancellation notice readable if additive card metadata is unavailable", () => {
    renderList({ messages: [message("notice", { role: "assistant", message_kind: "decision_cancel_notice", content: "Cancellation fallback" })] });
    expect(screen.getByText("Cancellation fallback")).toBeInTheDocument();
  });

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

describe("ChatMessageList — a card answer bubble", () => {
  const chosen = card({ chosen_option_index: 0, option_label: "Enabled" });

  it("option plus note: one chip and one note, no repeated option paragraph", () => {
    renderList({ messages: [message("a", { content: "Enabled\n\nEnabled. 17:31 ICT", decision_quotes: [chosen] })] });
    const bubble = screen.getByTestId("chat-user-message");
    expect(within(bubble).getAllByTestId("decision-answer-chip")).toHaveLength(1);
    expect(within(bubble).getByTestId("decision-answer-chip")).toHaveTextContent("Chose: Enabled");
    expect(within(bubble).getByTestId("decision-answer-note")).toHaveTextContent("Enabled. 17:31 ICT");
    expect(bubble.textContent).not.toMatch(/^Enabled\s*Enabled/);
  });

  it("option only: the chip and no note", () => {
    renderList({ messages: [message("a", { content: "Enabled", decision_quotes: [chosen] })] });
    const bubble = screen.getByTestId("chat-user-message");
    expect(within(bubble).getByTestId("decision-answer-chip")).toHaveTextContent("Chose: Enabled");
    expect(within(bubble).queryByTestId("decision-answer-note")).toBeNull();
  });

  it("text only: the text as today, no chip", () => {
    renderList({ messages: [message("a", { content: "Ship C instead.", decision_quotes: [card()] })] });
    const bubble = screen.getByTestId("chat-user-message");
    expect(bubble).toHaveTextContent("Ship C instead.");
    expect(within(bubble).queryByTestId("decision-answer-chip")).toBeNull();
  });
});
