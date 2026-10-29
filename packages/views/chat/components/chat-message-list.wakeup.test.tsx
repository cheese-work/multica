import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import type { ChatMessage, ChatPendingTask, TaskMessagePayload } from "@multica/core/types";
import { chatKeys } from "@multica/core/chat/queries";
import type { ReactElement } from "react";
import { NavigationProvider } from "../../navigation";
import { RESOURCES } from "../../locales";

vi.mock("react-virtuoso", () => ({
  Virtuoso: ({ data, itemContent, computeItemKey, components, context }: {
    data: unknown[];
    itemContent: (i: number, item: unknown) => ReactElement;
    computeItemKey: (i: number, item: unknown) => string;
    components?: { Footer?: (props: { context?: unknown }) => ReactElement };
    context?: unknown;
  }) => (
    <div>
      {data.map((item, i) => (
        <div key={computeItemKey(i, item)}>{itemContent(i, item)}</div>
      ))}
      {components?.Footer ? <components.Footer context={context} /> : null}
    </div>
  ),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({
    getActorName: () => "Terra",
    getActorInitials: () => "T",
    getActorAvatarUrl: () => null,
    hasActor: () => true,
  }),
}));

import { ChatMessageList } from "./chat-message-list";

const HEADER = "[Scheduled wakeup — you set this earlier in this DM; no person wrote this message.]";
const base = { chat_session_id: "s1", role: "user" as const, task_id: null, created_at: "2026-10-29T10:00:00Z" };

const queryClient = new QueryClient();

function listElement(messages: ChatMessage[], pendingTask: ChatPendingTask | null = null) {
  return (
    <I18nProvider locale="en" resources={RESOURCES}>
      <WorkspaceSlugProvider slug="acme">
        <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (p) => p }}>
          <QueryClientProvider client={queryClient}>
            <ChatMessageList messages={messages} pendingTask={pendingTask} availability="online" sessionId="s1" />
          </QueryClientProvider>
        </NavigationProvider>
      </WorkspaceSlugProvider>
    </I18nProvider>
  );
}

function renderList(messages: ChatMessage[], pendingTask: ChatPendingTask | null = null) {
  return render(listElement(messages, pendingTask));
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

  it("continues the rail into the replying agent's avatar for a wakeup task, and only for it", () => {
    const wakeup = { id: "w1", agent_id: "a1", title: "Report", instruction: "report", fire_at: "2026-10-29T10:00:00Z", created_at: "2026-10-29T03:10:00Z" };
    renderList([
      { ...base, id: "m1", content: "hello", task_id: "t0" },
      { ...base, id: "m2", content: `${HEADER}\nInstruction: report`, task_id: "t1", scheduled_wakeup: wakeup },
      { ...base, id: "m3", role: "assistant", content: "Here is the report", task_id: "t1" },
      { ...base, id: "m4", role: "assistant", content: "Unrelated reply", task_id: "t0" },
    ]);
    const replies = screen.getAllByTestId("wakeup-reply");
    expect(replies).toHaveLength(1);
    expect(replies[0]).toHaveTextContent("Here is the report");
    expect(screen.getByText("Unrelated reply").closest('[data-testid="wakeup-reply"]')).toBeNull();
  });

  it("keeps exactly one reply avatar from pending, through streaming, to the persisted reply", () => {
    const TASK = "0199c0de-0000-4000-8000-0000000000aa";
    const wakeup = { id: "w1", agent_id: "a1", title: "Report", instruction: "report", fire_at: "2026-10-29T10:00:00Z", created_at: "2026-10-29T03:10:00Z" };
    const trigger: ChatMessage = { ...base, id: "m2", content: `${HEADER}\nInstruction: report`, task_id: TASK, scheduled_wakeup: wakeup };
    const pending: ChatPendingTask = { task_id: TASK, status: "running", created_at: "2026-10-29T10:00:00Z" };
    const streamed: TaskMessagePayload = { task_id: TASK, issue_id: "", seq: 1, type: "text", content: "Working on the report" };
    const avatars = () => screen.getAllByTestId("wakeup-reply-avatar");

    // Pending: the status pill is the only reply row.
    queryClient.setQueryData(chatKeys.taskMessages(TASK), []);
    const view = renderList([trigger], pending);
    expect(avatars()).toHaveLength(1);
    const pillText = avatars()[0]!.closest('[data-testid="wakeup-reply"]')?.textContent ?? "";
    expect(pillText).not.toBe("");

    // Streaming: the live row and the pill both show, behind one avatar.
    queryClient.setQueryData(chatKeys.taskMessages(TASK), [streamed]);
    view.rerender(listElement([trigger], pending));
    expect(avatars()).toHaveLength(1);
    // Live row and pill share the reply's indent; only the live row carries the avatar.
    expect(screen.getAllByTestId("wakeup-reply")).toHaveLength(2);
    expect(screen.getByRole("status")).toBeInTheDocument();

    // Persisted: the reply takes over the one avatar.
    view.rerender(listElement([trigger, { ...base, id: "m3", role: "assistant", content: "Report done", task_id: TASK }], null));
    expect(avatars()).toHaveLength(1);
    expect(screen.getByText("Report done")).toBeInTheDocument();
  });
});
