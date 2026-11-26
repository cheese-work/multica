import { forwardRef, useImperativeHandle, type ReactElement } from "react";
import { describe, expect, it, vi } from "vitest";
import { render } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ChatMessage } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import { ChatAnnotationContext } from "./chat-annotation-context";

const { scrollToIndex } = vi.hoisted(() => ({ scrollToIndex: vi.fn() }));

vi.mock("react-virtuoso", () => ({
  Virtuoso: forwardRef(function Virtuoso({ data, itemContent, computeItemKey }: {
    data: unknown[];
    itemContent: (i: number, item: unknown) => ReactElement;
    computeItemKey: (i: number, item: unknown) => string;
  }, ref) {
    useImperativeHandle(ref, () => ({ scrollToIndex }));
    return <div>{data.map((item, i) => <div key={computeItemKey(i, item)}>{itemContent(i, item)}</div>)}</div>;
  }),
}));

import { ChatMessageList } from "./chat-message-list";

const message = (id: string, role: "user" | "assistant"): ChatMessage => ({
  id, chat_session_id: "s1", role, content: `text ${id}`, task_id: null, created_at: "2026-10-20T00:00:00Z",
});

function renderList(revealMessageId: string | null, onRevealMissing = vi.fn(), older: { hasOlderMessages?: boolean; onLoadOlderMessages?: () => void } = {}) {
  return render(
    <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
      <QueryClientProvider client={new QueryClient()}>
        <ChatAnnotationContext.Provider value={{ enabled: true, revealMessageId, onRevealMissing }}>
          <ChatMessageList
            messages={[message("u1", "user"), message("m1", "assistant"), message("m2", "assistant")]}
            pendingTask={null}
            availability="online"
            {...older}
          />
        </ChatAnnotationContext.Provider>
      </QueryClientProvider>
    </I18nProvider>,
  );
}

describe("chat message list reveal for an annotation edit", () => {
  it("scrolls the requested source message into view", () => {
    scrollToIndex.mockClear();
    renderList("m1");
    expect(scrollToIndex).toHaveBeenCalledWith(expect.objectContaining({ index: 1, align: "center" }));
  });

  it("reports a source that is not in the conversation", () => {
    const onRevealMissing = vi.fn();
    renderList("gone", onRevealMissing);
    expect(onRevealMissing).toHaveBeenCalledTimes(1);
  });

  it("loads older pages while the source is not loaded yet, and reports it missing once none are left", () => {
    const onRevealMissing = vi.fn();
    const onLoadOlderMessages = vi.fn();
    renderList("decision-request:c9", onRevealMissing, { hasOlderMessages: true, onLoadOlderMessages });
    expect(onLoadOlderMessages).toHaveBeenCalled();
    expect(onRevealMissing).not.toHaveBeenCalled();
  });
});
