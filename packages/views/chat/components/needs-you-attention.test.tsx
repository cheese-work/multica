import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { chatSessionsOptions } from "@multica/core/chat/queries";
import type { Agent, ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import { NavigationProvider } from "../../navigation";

const listChatSessions = vi.fn();
const listOpenDecisionRequests = vi.fn();

vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...await importOriginal<typeof import("@multica/core/paths")>(),
  useWorkspaceSlug: () => "test-ws",
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/api", () => ({
  api: {
    listChatSessions: (...args: unknown[]) => listChatSessions(...args),
    listOpenDecisionRequests: (...args: unknown[]) => listOpenDecisionRequests(...args),
  },
}));

import { NeedsYouList } from "./needs-you-list";

const NOW = Date.parse("2026-11-08T03:00:00Z");
const hoursAgo = (h: number) => new Date(NOW - h * 3_600_000).toISOString();

function make(overrides: Partial<ChatSession> & Pick<ChatSession, "id">): ChatSession {
  return {
    workspace_id: "ws-1",
    agent_id: "agent-1",
    creator_id: "user-1",
    title: `DM ${overrides.id}`,
    status: "active",
    has_unread: false,
    created_at: hoursAgo(100),
    updated_at: hoursAgo(1),
    stale_level: "none",
    attention_source: null,
    attention_since: null,
    next_change_at: null,
    ...overrides,
  };
}

const red = (id: string, hours: number, source: ChatSession["attention_source"] = "ask") =>
  make({ id, stale_level: "red", attention_source: source, attention_since: hoursAgo(hours) });
const yellow = (id: string, hours: number) =>
  make({ id, stale_level: "yellow", attention_source: "unseen", attention_since: hoursAgo(hours) });
const fresh = (id: string, hours: number) =>
  make({ id, stale_level: "fresh", attention_source: "later", attention_since: hoursAgo(hours) });

const agents = [{ id: "agent-1", name: "Alpha" } as unknown as Agent];

function Harness() {
  const { data } = useQuery(chatSessionsOptions("ws-1"));
  return <NeedsYouList sessions={data ?? []} agents={agents} />;
}

function renderBox() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
        <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/test-ws/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (p) => p }}>
          <Harness />
        </NavigationProvider>
      </I18nProvider>
    </QueryClientProvider>,
  );
  return client;
}

async function flush() {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
}

describe("Needs you box: attention rows", () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval", "Date"] });
    vi.setSystemTime(NOW);
    listChatSessions.mockReset();
    listOpenDecisionRequests.mockReset().mockResolvedValue([]);
  });
  afterEach(() => vi.useRealTimers());

  it("shows a plain one-line summary for a proposed action, never raw markdown", async () => {
    listChatSessions.mockResolvedValue([]);
    listOpenDecisionRequests.mockResolvedValue([{
      id: "dr-1", issue_id: "i-1", issue_identifier: "CHE-4678", asking_agent_id: "agent-1", asking_agent_name: "Alpha",
      ask: "**Proposed action**\n`workflow.steps.set` version 1 on [Issue](mention://issue/01a1bb73-0000-7000-8000-000000000000)\n\n**Parameters**\n```json\n{\n  \"step\": \"review\"\n}\n```",
      escalation_class: "other", created_at: hoursAgo(1), chat_session_id: "s-1", message_id: "m-1",
    }]);
    renderBox();
    await flush();
    const row = screen.getByRole("link", { name: /CHE-4678/ });
    expect(row.textContent).not.toContain("mention://");
    expect(row.textContent).not.toContain("](");
    expect(row.textContent).not.toContain("Parameters");
    expect(row.textContent).toContain("workflow.steps.set v1 on CHE-4678");
  });

  it("orders red, yellow, fresh, then oldest start, then id", async () => {
    listChatSessions.mockResolvedValue([
      fresh("f1", 1),
      red("r-new", 25),
      yellow("y1", 5),
      red("r-b", 30),
      red("r-a", 30),
      make({ id: "quiet" }),
    ]);
    renderBox();
    await flush();
    const links = screen.getAllByTestId("attention-row").map((el) => el.getAttribute("data-session-id"));
    expect(links).toEqual(["r-a", "r-b", "r-new", "y1", "f1"]);
    expect(screen.queryByText("DM quiet")).not.toBeInTheDocument();
  });

  it("gives each row a text label, an age and an accessible name", async () => {
    listChatSessions.mockResolvedValue([red("r1", 26)]);
    renderBox();
    await flush();
    const row = screen.getByRole("link", { name: /Red, waiting 26 hours/ });
    expect(row).toHaveAccessibleName(/Question: DM r1/);
    expect(within(row).getByText("Red · 26 hours")).toBeInTheDocument();
    expect(within(row).getByText("Question")).toBeInTheDocument();
    expect(within(row).getByText("Alpha")).toBeInTheDocument();
    expect(row).toHaveAttribute("href", expect.stringContaining("session=r1"));
  });

  it("shows 5 / N and reveals the rest on request", async () => {
    listChatSessions.mockResolvedValue(
      Array.from({ length: 7 }, (_, i) => red(`r${i}`, 30 - i)),
    );
    renderBox();
    await flush();
    expect(screen.getByRole("heading", { name: /Needs you\s+5 \/ 7/ })).toBeInTheDocument();
    expect(screen.getAllByTestId("attention-row")).toHaveLength(5);
    fireEvent.click(screen.getByRole("button", { name: "Show all 7" }));
    expect(screen.getAllByTestId("attention-row")).toHaveLength(7);
    expect(screen.getByRole("heading", { name: /Needs you\s+7 \/ 7/ })).toBeInTheDocument();
  });

  it("lists unclassified turns in a collapsed group with no colour and no age", async () => {
    listChatSessions.mockResolvedValue([
      make({ id: "u1", attention_unclassified_keys: ["turn:t1", "turn:t2"] }),
    ]);
    renderBox();
    await flush();
    const toggle = screen.getByRole("button", { name: /Unclassified/ });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveTextContent("2");
    expect(screen.queryByTestId("attention-row")).not.toBeInTheDocument();
    expect(screen.queryByTestId("unclassified-row")).not.toBeInTheDocument();
    // Not part of the stale count.
    expect(screen.getByRole("heading", { name: /Needs you\s+0/ })).toBeInTheDocument();
    fireEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    const row = screen.getByTestId("unclassified-row");
    expect(row).toHaveAttribute("data-subject-keys", "turn:t1 turn:t2");
    expect(row).not.toHaveTextContent(/Red|Yellow|New|hour|minute/);
  });

  // C1: `/later 2h` on a DM with no other obligation is quiet until due, then
  // appears without navigation or reload.
  it("C1: a later DM is quiet until next_change_at, then appears without reload", async () => {
    const due = new Date(NOW + 2 * 3_600_000).toISOString();
    listChatSessions.mockResolvedValueOnce([make({ id: "l1", next_change_at: due })]);
    listChatSessions.mockResolvedValueOnce([
      make({ id: "l1", stale_level: "fresh", attention_source: "later", attention_since: due }),
    ]);
    renderBox();
    await flush();
    expect(screen.queryByTestId("attention-row")).not.toBeInTheDocument();
    expect(listChatSessions).toHaveBeenCalledTimes(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2 * 3_600_000 - 1_000);
    });
    expect(listChatSessions).toHaveBeenCalledTimes(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_500);
    });
    expect(listChatSessions).toHaveBeenCalledTimes(2);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    expect(screen.getByTestId("attention-row")).toHaveAttribute("data-session-id", "l1");
    expect(screen.getByText("Reminder")).toBeInTheDocument();
  });

  // Server clock behind ours: the refetch returns the same next_change_at and
  // the row is still hidden. The timer must try again, a bounded number of times.
  it("retries a bounded number of times when next_change_at does not move", async () => {
    const due = new Date(NOW + 60_000).toISOString();
    const lagging = make({ id: "l1", next_change_at: due });
    listChatSessions.mockResolvedValue([lagging]);
    renderBox();
    await flush();
    expect(listChatSessions).toHaveBeenCalledTimes(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(61_000);
    });
    await flush();
    expect(listChatSessions).toHaveBeenCalledTimes(2);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(6_000);
    });
    await flush();
    expect(listChatSessions).toHaveBeenCalledTimes(3);

    // Bounded: it gives up instead of polling forever.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60 * 60_000);
    });
    await flush();
    expect(listChatSessions.mock.calls.length).toBeLessThanOrEqual(7);
  });

  it("refetches the list when the window regains focus", async () => {
    listChatSessions.mockResolvedValue([]);
    renderBox();
    await flush();
    expect(listChatSessions).toHaveBeenCalledTimes(1);
    await act(async () => {
      window.dispatchEvent(new Event("focus"));
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(listChatSessions).toHaveBeenCalledTimes(2);
  });

  // C2: acknowledge an old untyped turn, then finish a new unclassified turn:
  // the new turn appears and the old turn stays acknowledged. The server drops
  // an acknowledged turn key and returns the new one; the box shows exactly
  // what the server returned.
  it("C2: a new unclassified turn appears and the acknowledged one stays gone", async () => {
    listChatSessions.mockResolvedValueOnce([
      make({ id: "u1", attention_unclassified_keys: ["turn:old"] }),
    ]);
    listChatSessions.mockResolvedValueOnce([
      make({ id: "u1", attention_unclassified_keys: ["turn:new"] }),
    ]);
    const client = renderBox();
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /Unclassified/ }));
    expect(screen.getByTestId("unclassified-row")).toHaveAttribute("data-subject-keys", "turn:old");

    await act(async () => {
      await client.refetchQueries({ queryKey: chatSessionsOptions("ws-1").queryKey });
    });
    await flush();
    const row = screen.getByTestId("unclassified-row");
    expect(row).toHaveAttribute("data-subject-keys", "turn:new");
    expect(row.getAttribute("data-subject-keys")).not.toContain("turn:old");
    // The group stays open across the refetch.
    expect(screen.getByRole("button", { name: /Unclassified/ })).toHaveAttribute("aria-expanded", "true");
  });
});
