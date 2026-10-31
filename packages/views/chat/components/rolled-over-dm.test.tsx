import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { chatKeys } from "@multica/core/chat/queries";
import type { ChatMessage, ChatSession, TaskMessagePayload } from "@multica/core/types";
import type { ReactElement } from "react";
import enChat from "../../locales/en/chat.json";
import { NavigationProvider } from "../../navigation";

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

const OLD = "11111111-1111-4111-8111-111111111111";
const NEW = "22222222-2222-4222-8222-222222222222";
const TASK = "6af44cbe-80ab-4dfe-b07d-bd3cfd588f4d";
const session = (id: string, o: Partial<ChatSession> = {}): ChatSession => ({
  id, workspace_id: "ws", agent_id: "agent", creator_id: "u", title: "Old DM",
  status: "archived", has_unread: false, created_at: "2026-10-31T00:00:00Z", updated_at: "2026-10-31T00:00:00Z", ...o,
});
const sessions = [
  session(OLD, {
    continued_into_session_id: NEW,
    last_message: { content: "> quoted&nbsp;reply\n> more", role: "assistant", created_at: "2026-10-31T00:00:00Z" },
  }),
  session(NEW, { status: "active", continued_from_session_id: OLD }),
];
const summary: ChatMessage = {
  id: "sum", chat_session_id: NEW, role: "assistant", content: "Summary of the past",
  message_kind: "continuation_summary", task_id: null, created_at: "2026-10-31T00:00:00Z",
};
const finalReply: ChatMessage = {
  id: "reply", chat_session_id: NEW, role: "assistant", content: "Final reply text", task_id: TASK,
  created_at: "2026-10-31T00:01:00Z",
};
const steps: TaskMessagePayload[] = [
  { task_id: TASK, seq: 0, type: "tool_use", tool: "Bash", input: { command: "go test ./..." } },
  { task_id: TASK, seq: 1, type: "tool_result", tool: "Bash", output: "ok" },
  { task_id: TASK, seq: 2, type: "text", content: "Final reply text" },
] as TaskMessagePayload[];

function renderRolledOver(messages: ChatMessage[]) {
  const qc = new QueryClient();
  qc.setQueryData(chatKeys.messagesPage(OLD), { pages: [{ messages: [], limit: 50, has_more: false }], pageParams: [null] });
  qc.setQueryData(chatKeys.taskMessages(TASK), steps);
  return render(
    <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
      <WorkspaceSlugProvider slug="acme">
        <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (p) => p }}>
          <QueryClientProvider client={qc}>
            <ChatMessageList messages={messages} pendingTask={null} availability="online" sessionId={NEW}
              continuedFromSessionId={OLD} sessions={sessions} />
          </QueryClientProvider>
        </NavigationProvider>
      </WorkspaceSlugProvider>
    </I18nProvider>,
  );
}

describe("rolled-over DM opening (CHE-2767)", () => {
  it("previews the earlier DM as clamped plain text", () => {
    renderRolledOver([summary, finalReply]);
    fireEvent.click(screen.getByRole("button", { name: "Show earlier context" }));
    const preview = screen.getByTestId("predecessor-preview");
    expect(preview).toHaveTextContent("Terra: quoted reply more");
    expect(preview.textContent).not.toMatch(/[>&]/);
    expect(preview).toHaveClass("line-clamp-2");
    expect(preview).not.toHaveClass("truncate");
  });

  it("folds the link, summary and earlier DM into one collapsed continuation block", () => {
    renderRolledOver([summary, finalReply]);
    expect(screen.getByRole("link", { name: "previous DM" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Conversation summary" })).not.toBeInTheDocument();
    expect(screen.queryByText("Summary of the past")).not.toBeInTheDocument();
    expect(screen.queryByText(/Earlier DM/)).not.toBeInTheDocument();
    const expand = screen.getByRole("button", { name: "Show earlier context" });
    expect(expand).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(expand);
    expect(expand).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByText("Summary of the past")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Earlier DM · Old DM" })).toBeInTheDocument();
  });

  it("shows the old turn's final reply without its tool steps", () => {
    renderRolledOver([summary, finalReply]);
    expect(screen.getByText("Final reply text")).toBeInTheDocument();
    expect(screen.queryByText(/go test/)).not.toBeInTheDocument();
    expect(screen.queryByText(/steps?\b/i)).not.toBeInTheDocument();
  });

  it("keeps the steps of a turn the person asked for in this DM", () => {
    const asked: ChatMessage = { id: "u1", chat_session_id: NEW, role: "user", content: "Run it", task_id: null, created_at: "2026-10-31T00:00:30Z" };
    renderRolledOver([summary, asked, finalReply]);
    expect(screen.getAllByText(/step/i).length).toBeGreaterThan(0);
  });
});
