import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { chatKeys } from "@multica/core/chat/queries";
import type { ChatMessage, TaskMessagePayload } from "@multica/core/types";
import type { ReactElement } from "react";
import enChat from "../../locales/en/chat.json";
import { ChatAnnotationContext, type ChatAnnotationScope } from "./chat-annotation-context";

vi.mock("react-virtuoso", () => ({
  Virtuoso: ({ data, itemContent, computeItemKey }: {
    data: unknown[];
    itemContent: (i: number, item: unknown) => ReactElement;
    computeItemKey: (i: number, item: unknown) => string;
  }) => (
    <div>{data.map((item, i) => <div key={computeItemKey(i, item)}>{itemContent(i, item)}</div>)}</div>
  ),
}));

import { ChatMessageList } from "./chat-message-list";

const message = (id: string, role: "user" | "assistant", content: string): ChatMessage => ({
  id, chat_session_id: "s1", role, content, task_id: null, created_at: "2026-10-20T00:00:00Z",
  elapsed_ms: role === "assistant" ? 38000 : undefined,
});

function renderList(scope?: ChatAnnotationScope) {
  const list = (
    <ChatMessageList
      messages={[message("u1", "user", "Why does a claim fail?"), message("m1", "assistant", "The delay is fixed at 5 seconds.")]}
      pendingTask={null}
      availability="online"
    />
  );
  return render(
    <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
      <QueryClientProvider client={new QueryClient()}>
        {scope ? <ChatAnnotationContext.Provider value={scope}>{list}</ChatAnnotationContext.Provider> : list}
      </QueryClientProvider>
    </I18nProvider>,
  );
}

describe("annotatable agent reply", () => {
  it("wraps the settled reply in a named tab stop followed by its marker slot, then the footer", () => {
    renderList({ enabled: true, agentName: "Terra" });
    const region = screen.getByRole("group", { name: "Message from Terra" });
    expect(region).toHaveAttribute("data-comment-content", "m1");
    expect(region).toHaveAttribute("tabindex", "0");
    expect(region).toHaveTextContent("The delay is fixed at 5 seconds.");
    const slot = region.parentElement!.querySelector("[data-annotation-overlay-slot]")!;
    expect(region.compareDocumentPosition(slot) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    const copy = screen.getByTestId("assistant-message-copy");
    expect(slot.compareDocumentPosition(copy) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("leaves the reply plain when annotation is off", () => {
    const { container } = renderList();
    expect(container.querySelector("[data-comment-content]")).toBeNull();
    expect(screen.queryByRole("group", { name: /Message from/ })).not.toBeInTheDocument();
    expect(container.querySelector("[data-annotation-overlay-slot]")).toBeNull();
  });

  it("never offers annotation on the user's own message", () => {
    const { container } = renderList({ enabled: true, agentName: "Terra" });
    const own = screen.getByTestId("chat-user-message");
    expect(own).toHaveTextContent("Why does a claim fail?");
    expect(own.querySelector("[data-comment-content]")).toBeNull();
    expect(container.querySelectorAll("[data-comment-content]")).toHaveLength(1);
  });
});

describe("annotatable answer across the live to settled handoff", () => {
  const TASK_ID = "6af44cbe-80ab-4dfe-b07d-bd3cfd588f4d";
  const taskMsg = (seq: number, type: TaskMessagePayload["type"], extra: Partial<TaskMessagePayload> = {}) =>
    ({ task_id: TASK_ID, seq, type, ...extra }) as TaskMessagePayload;

  it("keeps the answer node mounted and only then becomes a selectable region", async () => {
    const qc = new QueryClient();
    qc.setQueryData(chatKeys.taskMessages(TASK_ID), [
      taskMsg(0, "tool_use", { tool: "Read", input: { path: "/tmp/x" } }),
      taskMsg(1, "text", { content: "Stable final answer" }),
    ]);
    const view = (messages: ChatMessage[], pendingTask: { task_id: string; status: "running" } | null) => (
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
        <QueryClientProvider client={qc}>
          <ChatAnnotationContext.Provider value={{ enabled: true, agentName: "Terra" }}>
            <ChatMessageList messages={messages} pendingTask={pendingTask} availability="online" />
          </ChatAnnotationContext.Provider>
        </QueryClientProvider>
      </I18nProvider>
    );
    const { rerender, container } = render(view([], { task_id: TASK_ID, status: "running" }));
    const answerBefore = await screen.findByText("Stable final answer");
    expect(container.querySelector("[data-comment-content]")).toBeNull();
    expect(screen.queryByRole("group", { name: /Message from/ })).not.toBeInTheDocument();

    rerender(view([{ ...message("m2", "assistant", "Stable final answer"), task_id: TASK_ID }], null));
    expect(await screen.findByText("Stable final answer")).toBe(answerBefore);
    expect(screen.getByRole("group", { name: "Message from Terra" })).toContainElement(answerBefore);
  });
});
