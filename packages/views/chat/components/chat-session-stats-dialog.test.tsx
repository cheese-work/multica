import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ChatSession, ChatSessionUsage } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";

const getUsage = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", () => ({
  api: { getChatSessionUsage: getUsage },
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));

import { ChatSessionStatsDialog } from "./chat-session-stats-dialog";

const TICKS_PER_USD = 10_000_000_000;
const zero = { input: 0, output: 0, cache_read: 0, cache_write: 0 };

function usage(over: Partial<ChatSessionUsage> = {}): ChatSessionUsage {
  return {
    session_id: "s1",
    tokens: { input: 150_000, output: 32_400, cache_read: 0, cache_write: 0 },
    uncosted_tokens: { input: 18_300, output: 0, cache_read: 0, cache_write: 0 },
    cost_usd_ticks: 0.4 * TICKS_PER_USD,
    runs: {
      by_status: { completed: 12, failed: 1, running: 1 },
      terminal: 13,
      metered: 11,
      unreported: 2,
      total_run_ms: 18 * 60_000 + 4_000,
    },
    messages: { by_role: { user: 14, assistant: 13 } },
    by_model: {
      "claude-opus-4-7": {
        tokens: { input: 100_000, output: 14_100, cache_read: 0, cache_write: 0 },
        cost_usd_ticks: 0.4 * TICKS_PER_USD,
      },
      "mystery-model": {
        tokens: { input: 18_300, output: 0, cache_read: 0, cache_write: 0 },
        cost_usd_ticks: 0,
      },
    },
    continued_from_session_id: null,
    continued_into_session_id: null,
    forked_from_session_id: null,
    ...over,
  };
}

const sessions = [
  { id: "prev", title: "Dashboard rollout" },
  { id: "src", title: "Billing export bug" },
  { id: "next", title: "Rollout, part two" },
] as ChatSession[];

function renderDialog(props: Partial<React.ComponentProps<typeof ChatSessionStatsDialog>> = {}) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const onOpenChange = vi.fn();
  render(
    <QueryClientProvider client={client}>
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
        <ChatSessionStatsDialog
          sessionId="s1"
          open
          onOpenChange={onOpenChange}
          sessions={sessions}
          {...props}
        />
      </I18nProvider>
    </QueryClientProvider>,
  );
  return { client, onOpenChange };
}

describe("ChatSessionStatsDialog", () => {
  beforeEach(() => getUsage.mockReset());

  it("shows an estimated, partial session total with runs, run time, messages and models", async () => {
    getUsage.mockResolvedValue(usage());
    renderDialog();

    const dialog = await screen.findByTestId("chat-stats-dialog");
    expect(await within(dialog).findByTestId("chat-stats-cost")).toHaveTextContent("$0.40");
    expect(dialog).toHaveTextContent("Estimated");
    expect(dialog).toHaveTextContent("Partial");
    expect(dialog).toHaveTextContent("Usage missing for 2 finished runs");
    expect(dialog).toHaveTextContent("182.4K tokens");
    expect(dialog).toHaveTextContent("12 completed · 1 failed");
    expect(dialog).toHaveTextContent("1 run in progress");
    expect(dialog).toHaveTextContent("18m 4s");
    expect(dialog).toHaveTextContent("You 14 · Agent 13");
    const opus = within(dialog).getByRole("row", { name: /claude-opus-4-7/ });
    expect(opus).toHaveTextContent("$0.40");
    const mystery = within(dialog).getByRole("row", { name: /mystery-model/ });
    expect(mystery).toHaveTextContent("—");
    expect(dialog).not.toHaveTextContent("Whole conversation");
  });

  it("shows a skeleton in the final layout while loading", async () => {
    let release: (value: ChatSessionUsage) => void = () => {};
    getUsage.mockReturnValue(new Promise<ChatSessionUsage>((resolve) => (release = resolve)));
    renderDialog();
    expect(await screen.findByTestId("chat-stats-state-loading")).toBeInTheDocument();
    release(usage());
    expect(await screen.findByTestId("chat-stats-cost")).toBeInTheDocument();
    expect(screen.queryByTestId("chat-stats-state-loading")).not.toBeInTheDocument();
  });

  it("shows a dash, never $0, when no usage was recorded", async () => {
    getUsage.mockResolvedValue(
      usage({
        tokens: zero,
        uncosted_tokens: zero,
        cost_usd_ticks: 0,
        runs: { by_status: {}, terminal: 0, metered: 0, unreported: 0, total_run_ms: 0 },
        messages: { by_role: { user: 1 } },
        by_model: {},
      }),
    );
    renderDialog();

    const empty = await screen.findByTestId("chat-stats-state-empty");
    expect(empty).toHaveTextContent("No runs yet.");
    expect(screen.getByTestId("chat-stats-cost")).toHaveTextContent("—");
    expect(screen.getByTestId("chat-stats-dialog")).not.toHaveTextContent("$0");
    expect(screen.getByTestId("chat-stats-dialog")).toHaveTextContent("You 1 · Agent 0");
  });

  it("shows an error with Retry and never zeros, and retries on request", async () => {
    getUsage.mockRejectedValueOnce(new Error("boom")).mockResolvedValueOnce(usage());
    renderDialog();

    const error = await screen.findByTestId("chat-stats-state-error");
    expect(error).toHaveTextContent("Statistics could not be loaded. Nothing was recorded as zero.");
    expect(screen.queryByTestId("chat-stats-cost")).not.toBeInTheDocument();

    fireEvent.click(within(error).getByRole("button", { name: "Retry" }));
    expect(await screen.findByTestId("chat-stats-cost")).toHaveTextContent("$0.40");
    expect(getUsage).toHaveBeenCalledTimes(2);
  });

  it("keeps the last numbers visible and dimmed with Retry when a refresh fails", async () => {
    getUsage.mockResolvedValueOnce(usage());
    const { client } = renderDialog();
    expect(await screen.findByTestId("chat-stats-cost")).toHaveTextContent("$0.40");

    getUsage.mockRejectedValueOnce(new Error("offline"));
    await client.refetchQueries({ queryKey: ["chat", "ws-1", "usage", "s1"] });

    const offline = await screen.findByTestId("chat-stats-state-offline");
    expect(offline).toHaveTextContent("You're offline. These numbers are from when you opened Statistics");
    expect(within(offline).getByRole("button", { name: "Retry" })).toBeInTheDocument();
    expect(screen.getByTestId("chat-stats-cost")).toHaveTextContent("$0.40");
    expect(screen.getByTestId("chat-stats-body")).toHaveAttribute("data-stale", "true");
  });

  it("does not fetch while closed", () => {
    getUsage.mockResolvedValue(usage());
    renderDialog({ open: false });
    expect(getUsage).not.toHaveBeenCalled();
  });

  it("closes from the Close button", async () => {
    getUsage.mockResolvedValue(usage());
    const { onOpenChange } = renderDialog();
    fireEvent.click(await screen.findByTestId("chat-stats-close"));
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("links the related DMs by title and opens one", async () => {
    getUsage.mockResolvedValue(
      usage({ continued_from_session_id: "prev", forked_from_session_id: "src" }),
    );
    const onOpenSession = vi.fn();
    const { onOpenChange } = renderDialog({ onOpenSession });

    const dialog = await screen.findByTestId("chat-stats-dialog");
    await waitFor(() => expect(dialog).toHaveTextContent("Continued from"));
    expect(dialog).toHaveTextContent("Forked from");
    fireEvent.click(within(dialog).getByRole("button", { name: "Dashboard rollout" }));
    expect(onOpenSession).toHaveBeenCalledWith(sessions[0]);
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("breaks tokens into input, output, cache read and cache write, in total and per model", async () => {
    getUsage.mockResolvedValue(
      usage({
        tokens: { input: 150_000, output: 32_400, cache_read: 12_000, cache_write: 3_000 },
        by_model: {
          "claude-opus-4-7": {
            tokens: { input: 100_000, output: 14_100, cache_read: 8_000, cache_write: 2_000 },
            cost_usd_ticks: 0.4 * TICKS_PER_USD,
          },
        },
      }),
    );
    renderDialog();

    const total = await screen.findByTestId("chat-stats-token-breakdown");
    expect(total).toHaveTextContent("Input150K");
    expect(total).toHaveTextContent("Output32.4K");
    expect(total).toHaveTextContent("Cache read12K");
    expect(total).toHaveTextContent("Cache write3K");

    const dialog = screen.getByTestId("chat-stats-dialog");
    const headers = within(dialog).getAllByRole("columnheader").map((h) => h.textContent);
    expect(headers).toEqual(["Model", "Input", "Output", "Cache read", "Cache write", "Cost"]);
    const cells = within(within(dialog).getByRole("row", { name: /claude-opus-4-7/ }))
      .getAllByRole("cell")
      .map((c) => c.textContent);
    expect(cells).toEqual(["100K", "14.1K", "8K", "2K", "$0.40"]);
  });

  it("links the newer DM that continues this one and opens it", async () => {
    getUsage.mockResolvedValue(usage({ continued_into_session_id: "next" }));
    const onOpenSession = vi.fn();
    renderDialog({ onOpenSession });

    const dialog = await screen.findByTestId("chat-stats-dialog");
    await waitFor(() => expect(dialog).toHaveTextContent("Continued in"));
    fireEvent.click(within(dialog).getByRole("button", { name: "Rollout, part two" }));
    expect(onOpenSession).toHaveBeenCalledWith(sessions[2]);
  });
});
