import type { ReactNode } from "react";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ChatWakeup } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";

const mocks = vi.hoisted(() => ({ list: vi.fn(), cancel: vi.fn(), error: vi.fn() }));
vi.mock("@multica/core/api", () => ({
  api: { listChatSessionWakeups: mocks.list, cancelChatSessionWakeup: mocks.cancel },
}));
vi.mock("sonner", () => ({ toast: { error: mocks.error } }));

import { ScheduledWakeupsChip } from "./scheduled-wakeups-chip";

const wakeup = (id: string, instruction: string, fireAt: string): ChatWakeup => ({
  id,
  agent_id: "agent-1",
  fire_at: fireAt,
  instruction,
  status: "pending",
});

function renderChip() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>{children}</I18nProvider>
    </QueryClientProvider>
  );
  return render(<ScheduledWakeupsChip sessionId="s1" agentName="Sol" />, { wrapper });
}

describe("ScheduledWakeupsChip", () => {
  beforeEach(() => {
    mocks.list.mockReset();
    mocks.cancel.mockReset();
    mocks.error.mockReset();
  });

  it("renders nothing when no wakeup is pending", async () => {
    mocks.list.mockResolvedValue([]);
    const { container } = renderChip();
    await waitFor(() => expect(mocks.list).toHaveBeenCalledWith("s1"));
    expect(container).toBeEmptyDOMElement();
  });

  it("shows the pending count for one wakeup", async () => {
    mocks.list.mockResolvedValue([wakeup("w1", "Check the deploy", "2026-10-29T07:00:00Z")]);
    renderChip();
    expect(await screen.findByRole("button", { name: "Scheduled 1" })).toBeInTheDocument();
  });

  it("lists time, first instruction line and agent for many wakeups", async () => {
    mocks.list.mockResolvedValue([
      wakeup("w1", "Check the deploy\nthen report", "2026-10-29T07:00:00Z"),
      wakeup("w2", "Ping Cheese", "2026-10-29T09:30:00Z"),
      wakeup("w3", "Nightly sweep", "2026-10-29T14:00:00Z"),
    ]);
    renderChip();
    fireEvent.click(await screen.findByRole("button", { name: "Scheduled 3" }));
    expect(await screen.findByText("Check the deploy")).toBeInTheDocument();
    expect(screen.queryByText(/then report/)).not.toBeInTheDocument();
    expect(screen.getByText("Ping Cheese")).toBeInTheDocument();
    expect(screen.getByText("Scheduled by Sol")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: /^Cancel scheduled follow-up/ })).toHaveLength(3);
  });

  it("removes a cancelled row without a reload", async () => {
    mocks.list.mockResolvedValue([
      wakeup("w1", "First", "2026-10-29T07:00:00Z"),
      wakeup("w2", "Second", "2026-10-29T08:00:00Z"),
    ]);
    mocks.cancel.mockResolvedValue(undefined);
    renderChip();
    fireEvent.click(await screen.findByRole("button", { name: "Scheduled 2" }));
    mocks.list.mockResolvedValue([wakeup("w2", "Second", "2026-10-29T08:00:00Z")]);
    fireEvent.click((await screen.findAllByRole("button", { name: /^Cancel scheduled follow-up/ }))[0]!);
    await waitFor(() => expect(mocks.cancel).toHaveBeenCalledWith("s1", "w1"));
    await waitFor(() => expect(screen.queryByText("First")).not.toBeInTheDocument());
    expect(screen.getByText("Second")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Scheduled 1" })).toBeInTheDocument();
  });

  it("keeps the row and reports when cancel fails", async () => {
    mocks.list.mockResolvedValue([wakeup("w1", "First", "2026-10-29T07:00:00Z")]);
    mocks.cancel.mockRejectedValue(new Error("offline"));
    renderChip();
    fireEvent.click(await screen.findByRole("button", { name: "Scheduled 1" }));
    fireEvent.click(await screen.findByRole("button", { name: /^Cancel scheduled follow-up/ }));
    await waitFor(() => expect(mocks.error).toHaveBeenCalledWith(enChat.scheduled.cancel_failed));
    expect(screen.getByText("First")).toBeInTheDocument();
  });

  it("shows a retry control when the list fails to load, then the chip once it loads", async () => {
    mocks.list.mockRejectedValueOnce(new Error("schedule service unavailable"));
    renderChip();

    const retry = await screen.findByRole("button", { name: /couldn't load/i });
    mocks.list.mockResolvedValue([wakeup("w1", "Check the deploy", "2026-10-29T08:00:00Z")]);
    fireEvent.click(retry);

    expect(await screen.findByRole("button", { name: /Scheduled 1/ })).toBeTruthy();
    expect(mocks.list).toHaveBeenCalledTimes(2);
  });

  it("keeps the list when a background refetch fails", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    mocks.list.mockResolvedValue([wakeup("w1", "Check the deploy", "2026-10-29T08:00:00Z")]);
    render(
      <QueryClientProvider client={client}>
        <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
          <ScheduledWakeupsChip sessionId="s1" agentName="Sol" />
        </I18nProvider>
      </QueryClientProvider>,
    );
    await screen.findByRole("button", { name: /Scheduled 1/ });

    mocks.list.mockRejectedValue(new Error("blip"));
    await client.invalidateQueries();

    expect(screen.getByRole("button", { name: /Scheduled 1/ })).toBeTruthy();
  });
});
