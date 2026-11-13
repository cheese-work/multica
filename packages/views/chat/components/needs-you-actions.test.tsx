import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { chatSessionsOptions } from "@multica/core/chat/queries";
import type { Agent, ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import { NavigationProvider } from "../../navigation";

const listChatSessions = vi.fn();
const listOpenDecisionRequests = vi.fn();
const chatAttentionXong = vi.fn();
const chatAttentionLater = vi.fn();
const chatAttentionXongAll = vi.fn();
const chatAttentionUndoXongAll = vi.fn();
const toastFn = vi.fn();
const toastError = vi.fn();

vi.mock("sonner", () => ({ toast: Object.assign((...a: unknown[]) => toastFn(...a), { error: (...a: unknown[]) => toastError(...a) }) }));
vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...await importOriginal<typeof import("@multica/core/paths")>(),
  useWorkspaceSlug: () => "test-ws",
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/api", () => ({
  api: {
    listChatSessions: (...a: unknown[]) => listChatSessions(...a),
    listOpenDecisionRequests: (...a: unknown[]) => listOpenDecisionRequests(...a),
    chatAttentionXong: (...a: unknown[]) => chatAttentionXong(...a),
    chatAttentionLater: (...a: unknown[]) => chatAttentionLater(...a),
    chatAttentionXongAll: (...a: unknown[]) => chatAttentionXongAll(...a),
    chatAttentionUndoXongAll: (...a: unknown[]) => chatAttentionUndoXongAll(...a),
  },
}));

import { NeedsYouList } from "./needs-you-list";

function make(id: string, keys: string[] | undefined): ChatSession {
  return {
    id,
    workspace_id: "ws-1",
    agent_id: "agent-1",
    creator_id: "user-1",
    title: `DM ${id}`,
    status: "active",
    has_unread: false,
    created_at: "2026-11-08T00:00:00Z",
    updated_at: "2026-11-08T00:00:00Z",
    stale_level: "red",
    attention_source: "ask",
    attention_since: "2026-11-07T00:00:00Z",
    next_change_at: null,
    attention_keys: keys,
  };
}

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
}

describe("Needs you box: row actions", () => {
  beforeEach(() => {
    for (const f of [listChatSessions, chatAttentionXong, chatAttentionLater, chatAttentionXongAll, chatAttentionUndoXongAll, toastFn, toastError]) f.mockReset();
    listOpenDecisionRequests.mockReset().mockResolvedValue([]);
    chatAttentionXong.mockResolvedValue({});
    chatAttentionLater.mockResolvedValue({});
    chatAttentionXongAll.mockResolvedValue({ batch_id: "b1", attention: [] });
    chatAttentionUndoXongAll.mockResolvedValue({ batch_id: "b1", attention: [] });
  });

  const row = (id: string) => screen.getByTestId("attention-row-container-" + id);

  it("Done sends the row's displayed keys, minus decision keys, and refetches", async () => {
    listChatSessions.mockResolvedValue([make("s1", ["item:1", "decision:9@2", "turn:3"])]);
    renderBox();
    await screen.findByTestId("attention-row");
    fireEvent.click(within(row("s1")).getByRole("button", { name: /Mark done: DM s1/ }));
    await waitFor(() => expect(chatAttentionXong).toHaveBeenCalledWith("s1", ["item:1", "turn:3"]));
    await waitFor(() => expect(listChatSessions).toHaveBeenCalledTimes(2));
  });

  it("Later sends every displayed key with a request id and the server default duration", async () => {
    listChatSessions.mockResolvedValue([make("s1", ["item:1", "decision:9@2"])]);
    renderBox();
    await screen.findByTestId("attention-row");
    fireEvent.click(within(row("s1")).getByRole("button", { name: /Remind me later: DM s1/ }));
    await waitFor(() => expect(chatAttentionLater).toHaveBeenCalledTimes(1));
    const [sid, keys, opts] = chatAttentionLater.mock.calls[0]!;
    expect(sid).toBe("s1");
    expect(keys).toEqual(["item:1", "decision:9@2"]);
    expect(opts.durationSeconds).toBeUndefined();
    expect(typeof opts.requestId).toBe("string");
    expect(opts.requestId.length).toBeGreaterThan(0);
  });

  it("shows no row actions when the server sent no keys (older backend)", async () => {
    listChatSessions.mockResolvedValue([make("s1", undefined)]);
    renderBox();
    await screen.findByTestId("attention-row");
    expect(screen.queryByRole("button", { name: /Mark done/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Done all/ })).not.toBeInTheDocument();
  });

  it("A11: Done all sends all listed keys once and the toast Undo reopens the batch", async () => {
    listChatSessions.mockResolvedValue([make("s1", ["item:1"]), make("s2", ["turn:2", "decision:3@1"])]);
    renderBox();
    await screen.findAllByTestId("attention-row");
    fireEvent.click(screen.getByRole("button", { name: "Done all" }));
    await waitFor(() => expect(chatAttentionXongAll).toHaveBeenCalledWith(["item:1", "turn:2", "decision:3@1"]));
    await waitFor(() => expect(toastFn).toHaveBeenCalled());
    const [message, options] = toastFn.mock.calls[0]! as [string, { action: { label: string; onClick: () => void } }];
    expect(message).toBe("Marked 2 DMs done");
    expect(options.action.label).toBe("Undo");
    await act(async () => options.action.onClick());
    await waitFor(() => expect(chatAttentionUndoXongAll).toHaveBeenCalledWith("b1"));
  });

  it("reports a failed action and keeps the row", async () => {
    listChatSessions.mockResolvedValue([make("s1", ["item:1"])]);
    chatAttentionXong.mockRejectedValue(new Error("boom"));
    renderBox();
    await screen.findByTestId("attention-row");
    fireEvent.click(screen.getByRole("button", { name: /Mark done: DM s1/ }));
    await waitFor(() => expect(toastError).toHaveBeenCalledWith("Could not update. Try again."));
    expect(screen.getByTestId("attention-row")).toBeInTheDocument();
  });
});
