import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import type { ChatMessage } from "@multica/core/types";
import type { ReactElement } from "react";
import { NavigationProvider } from "../../navigation";
import { RESOURCES } from "../../locales";

vi.mock("react-virtuoso", () => ({
  Virtuoso: ({ data, itemContent, computeItemKey }: {
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
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: () => "Terra" }),
}));

import { ChatMessageList } from "./chat-message-list";

const HEADER = "[Scheduled wakeup — you set this earlier in this DM; no person wrote this message.]";
const base = { chat_session_id: "s1", role: "user" as const, task_id: null, created_at: "2026-10-29T10:00:00Z" };

function renderList(messages: ChatMessage[]) {
  return render(
    <I18nProvider locale="en" resources={RESOURCES}>
      <WorkspaceSlugProvider slug="acme">
        <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (p) => p }}>
          <QueryClientProvider client={new QueryClient()}>
            <ChatMessageList messages={messages} pendingTask={null} availability="online" sessionId="s1" />
          </QueryClientProvider>
        </NavigationProvider>
      </WorkspaceSlugProvider>
    </I18nProvider>,
  );
}

describe("ChatMessageList fired wakeups", () => {
  it("renders a message with a scheduled_wakeup as an event row, not a user bubble", () => {
    renderList([
      { ...base, id: "m1", content: "Remind me at 17:00" },
      {
        ...base,
        id: "m2",
        content: `${HEADER}\nInstruction: report`,
        scheduled_wakeup: { id: "w1", agent_id: "a1", title: "Report", instruction: "report", fire_at: "2026-10-29T10:00:00Z", created_at: "2026-10-29T03:10:00Z" },
      },
    ]);
    expect(screen.getByRole("img", { name: "Scheduled wakeup fired" })).toBeInTheDocument();
    expect(screen.getAllByTestId("chat-user-message")).toHaveLength(1);
    expect(screen.getByText("Remind me at 17:00")).toBeInTheDocument();
    expect(screen.queryByText(/Scheduled wakeup —/)).not.toBeInTheDocument();
  });

  it("never prints the model-facing header when the structured wakeup is missing", () => {
    renderList([{ ...base, id: "m2", content: `${HEADER}\nInstruction: report on CHE-1` }]);
    expect(screen.queryByRole("img", { name: "Scheduled wakeup fired" })).not.toBeInTheDocument();
    expect(screen.getByTestId("chat-user-message")).toHaveTextContent("report on CHE-1");
    expect(screen.queryByText(/Scheduled wakeup/)).not.toBeInTheDocument();
  });

  it("leaves ordinary user messages as bubbles", () => {
    renderList([{ ...base, id: "m1", content: "hello" }]);
    expect(screen.getByTestId("chat-user-message")).toHaveTextContent("hello");
    expect(screen.queryByRole("img", { name: "Scheduled wakeup fired" })).not.toBeInTheDocument();
  });
});
