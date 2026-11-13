import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { ChatSupplement } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";

const resend = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", () => ({
  api: { resendChatSupplement: resend },
  ApiError: class ApiError extends Error {},
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn() } }));

import { ChatSupplementStatus } from "./chat-supplement-status";

const base: ChatSupplement = {
  message_id: "m1",
  chat_session_id: "s1",
  task_id: "t1",
  status: "pending",
  failure_reason: null,
  delivered_at: null,
  acked_after_resend_at: null,
  resent_task_id: null,
};

function renderStatus(supplement: ChatSupplement) {
  const qc = new QueryClient();
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
        <ChatSupplementStatus supplement={supplement} />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => resend.mockReset());

describe("ChatSupplementStatus", () => {
  it.each(["pending", "delivering"] as const)("shows Added to run while %s", (status) => {
    renderStatus({ ...base, status });
    expect(screen.getByText("Added to run")).toBeInTheDocument();
  });

  it("shows Read by agent once delivered", () => {
    renderStatus({ ...base, status: "delivered", delivered_at: "2026-11-01T00:00:00Z" });
    expect(screen.getByText("Read by agent")).toBeInTheDocument();
  });

  it("states the possible duplicate when the ack landed after a resend", () => {
    renderStatus({
      ...base,
      status: "delivered",
      acked_after_resend_at: "2026-11-01T00:05:00Z",
      resent_task_id: "t2",
    });
    expect(
      screen.getByText("Read by agent; also sent again, the agent may see it twice"),
    ).toBeInTheDocument();
    expect(screen.queryByText("Read by agent")).not.toBeInTheDocument();
  });

  it("offers Send as a new run for a message that was never received, with no dialog", async () => {
    resend.mockResolvedValue({ task_id: "t2", supplement: { ...base, status: "failed", resent_task_id: "t2" } });
    renderStatus({ ...base, status: "failed", failure_reason: "turn_ended" });
    expect(screen.getByText("Not received")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Send as a new run" }));
    await waitFor(() => expect(resend).toHaveBeenCalledWith("s1", "m1", { confirmPossibleDuplicate: false }));
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
  });

  it("never labels an uncertain receipt Not received", () => {
    renderStatus({ ...base, status: "uncertain", failure_reason: "timeout" });
    expect(screen.getByText("Delivery unconfirmed")).toBeInTheDocument();
    expect(screen.queryByText("Not received")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Send as a new run" })).not.toBeInTheDocument();
  });

  it("sends the confirmation only after the user accepts the duplicate warning", async () => {
    resend.mockResolvedValue({ task_id: "t2", supplement: { ...base, status: "uncertain", resent_task_id: "t2" } });
    renderStatus({ ...base, status: "uncertain", failure_reason: "timeout" });
    fireEvent.click(screen.getByRole("button", { name: "Send again anyway" }));
    expect(
      await screen.findByText(
        "The agent may already have received this message. Sending it again can make the agent act on it twice.",
      ),
    ).toBeInTheDocument();
    expect(resend).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Send again" }));
    await waitFor(() => expect(resend).toHaveBeenCalledWith("s1", "m1", { confirmPossibleDuplicate: true }));
  });

  it("sends nothing when the user cancels the duplicate warning", async () => {
    renderStatus({ ...base, status: "uncertain" });
    fireEvent.click(screen.getByRole("button", { name: "Send again anyway" }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));
    expect(resend).not.toHaveBeenCalled();
  });

  it("shows no resend action once the message already has a resent task", () => {
    renderStatus({ ...base, status: "uncertain", resent_task_id: "t2" });
    expect(screen.queryByRole("button", { name: "Send again anyway" })).not.toBeInTheDocument();
    expect(screen.getByText("Sent again as a new run")).toBeInTheDocument();
  });
});
