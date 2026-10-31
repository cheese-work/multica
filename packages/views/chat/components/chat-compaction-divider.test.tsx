import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { NavigationProvider } from "../../navigation";
import type { ChatMessage } from "@multica/core/types";
import type { ReactElement } from "react";
import enChat from "../../locales/en/chat.json";

// Real react-virtuoso renders no data rows under jsdom's zero-height viewport.
vi.mock("react-virtuoso", () => ({
  Virtuoso: ({
    data,
    itemContent,
    computeItemKey,
  }: {
    data: unknown[];
    itemContent: (i: number, item: unknown) => ReactElement;
    computeItemKey: (i: number, item: unknown) => string;
  }) => (
    <div>
      {data.map((item, i) => (
        <div key={computeItemKey(i, item)}>{itemContent(i, item)}</div>
      ))}
    </div>
  ),
}));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => <span /> }));
vi.mock("@multica/core/workspace/hooks", () => ({ useActorName: () => ({ getActorName: () => "Terra" }) }));

import { ChatMessageList } from "./chat-message-list";

const SESSION = "33333333-3333-4333-8333-333333333333";

function message(over: Partial<ChatMessage>): ChatMessage {
  return {
    id: "m", chat_session_id: SESSION, role: "assistant", content: "", task_id: null,
    created_at: "2026-10-28T02:18:00Z", ...over,
  };
}

const summary = message({
  id: "sum", content: "Goal: ship in-place compaction.\n\nDecision: keep the evidence.",
  message_kind: "compaction_summary", task_id: "6af44cbe-80ab-4dfe-b07d-bd3cfd588f4d",
});

function renderList(messages: ChatMessage[]) {
  return render(
    <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
      <WorkspaceSlugProvider slug="acme">
        <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (path) => path }}>
          <QueryClientProvider client={new QueryClient()}>
            <ChatMessageList messages={messages} pendingTask={null} availability="online" sessionId={SESSION} />
          </QueryClientProvider>
        </NavigationProvider>
      </WorkspaceSlugProvider>
    </I18nProvider>,
  );
}

// U1: the divider is a fixed Asia/Bangkok wall time, whatever zone the reader's
// browser is in. Run with TZ=America/New_York: 02:18Z is 22:18 there, 09:18 ICT.
describe("compaction divider", () => {
  it("reads 'Compacted at HH:MM ICT' in Asia/Bangkok and keeps the summary collapsed", () => {
    renderList([
      message({ id: "old", role: "user", content: "Older question" }),
      summary,
      message({ id: "new", content: "Newer answer" }),
    ]);
    const toggle = screen.getByRole("button", { name: /Compacted at 09:18 ICT/ });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByText(/Decision: keep the evidence\./)).toBeNull();
    // Older messages stay in the list, above the divider.
    const older = screen.getByText("Older question");
    expect(older.compareDocumentPosition(toggle) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("expands to show the summary", () => {
    renderList([summary]);
    fireEvent.click(screen.getByRole("button", { name: /Compacted at 09:18 ICT/ }));
    expect(screen.getByText(/Decision: keep the evidence\./)).toBeInTheDocument();
  });

  it("does not draw the divider as a reply bubble", () => {
    renderList([summary]);
    expect(screen.queryByTestId("agent-avatar")).toBeNull();
    expect(screen.queryByText("Compacted at 09:18 ICT")?.closest("[data-testid='chat-user-message']") ?? null).toBeNull();
  });

  it("falls back to a plain 'Compacted' label when the time is unparseable", () => {
    renderList([{ ...summary, created_at: "not-a-time" }]);
    expect(screen.getByRole("button", { name: "Compacted" })).toBeInTheDocument();
  });
});
