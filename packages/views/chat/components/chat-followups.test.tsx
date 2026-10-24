import { fireEvent, render, screen, within } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ChatWakeup } from "@multica/core/types";
import { describe, expect, it, vi } from "vitest";
import enChat from "../../locales/en/chat.json";
import { ChatFollowUps } from "./chat-followups";

const TEST_RESOURCES = { en: { chat: enChat } };
const ICT = "Asia/Ho_Chi_Minh";
const NOW = new Date("2026-10-21T02:00:00Z"); // 09:00 ICT, same day as the follow-ups

const timeWakeup: ChatWakeup = {
  id: "w1",
  chat_session_id: "s1",
  instruction: "Check CHE-1812 and tell you the result.",
  fire_at: "2026-10-21T04:40:00Z", // 11:40 ICT
  status: "pending",
  task_id: null,
  condition: null,
};
const conditionWakeup: ChatWakeup = {
  ...timeWakeup,
  id: "w2",
  instruction: "Tell you what the review found.",
  condition: {
    issue_id: "i1",
    identifier: "CHE-1812",
    check: { type: "issue_field", field: "status", value: "done" },
  },
};

function renderStrip(props: Partial<React.ComponentProps<typeof ChatFollowUps>> = {}) {
  const onCancel = vi.fn();
  const onRetry = vi.fn();
  const view = render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <ChatFollowUps
        sessionId="s1"
        agentName="Ada"
        wakeups={[timeWakeup]}
        status="ready"
        timeZone={ICT}
        now={NOW}
        onCancel={onCancel}
        onRetry={onRetry}
        {...props}
      />
    </I18nProvider>,
  );
  return { onCancel, onRetry, container: view.container };
}

describe("ChatFollowUps", () => {
  it("renders nothing when no follow-up is pending", () => {
    const { container } = renderStrip({ wakeups: [] });
    expect(container).toBeEmptyDOMElement();
  });

  it("shows a visible group label, the time, the instruction and Cancel for a time follow-up", () => {
    renderStrip();
    const region = screen.getByRole("region", { name: "Scheduled follow-ups (1)" });
    expect(within(region).getByText("Scheduled follow-ups")).toBeVisible();
    expect(within(region).getByText(/Ada checks back 11:40 ICT/)).toBeInTheDocument();
    expect(within(region).getByText("Check CHE-1812 and tell you the result.")).toBeInTheDocument();
    expect(within(region).getByRole("button", { name: /^Cancel follow-up: Ada checks back 11:40/ })).toBeEnabled();
  });

  it("says when the condition holds or by the deadline for a conditional follow-up", () => {
    renderStrip({ wakeups: [conditionWakeup] });
    expect(screen.getByText(/Ada checks back when CHE-1812 is done, or by 11:40/)).toBeInTheDocument();
  });

  it("describes pull request and comment conditions", () => {
    renderStrip({
      wakeups: [
        { ...timeWakeup, id: "a", condition: { issue_id: "i", identifier: "CHE-7", check: { type: "pull_request", event: "checks_finished" } } },
        { ...timeWakeup, id: "b", condition: { issue_id: "i", identifier: "CHE-7", check: { type: "pull_request", event: "merged" } } },
        { ...timeWakeup, id: "c", condition: { issue_id: "i", identifier: "CHE-7", comment_by: { type: "member", id: "u1" } } },
      ],
    });
    expect(screen.getByText(/when the checks finish on CHE-7, or by/)).toBeInTheDocument();
    expect(screen.getByText(/when the pull request of CHE-7 merges, or by/)).toBeInTheDocument();
    expect(screen.getByText(/when there is a new comment on CHE-7, or by/)).toBeInTheDocument();
  });

  it("adds the date when the follow-up is not today", () => {
    renderStrip({ wakeups: [{ ...timeWakeup, fire_at: "2026-10-22T04:40:00Z" }] });
    expect(screen.getByText(/Ada checks back Oct 22, 11:40 ICT/)).toBeInTheDocument();
  });

  it("marks a follow-up set in the previous DM", () => {
    renderStrip({ wakeups: [{ ...timeWakeup, chat_session_id: "s0" }] });
    expect(screen.getByText(/Set in the previous DM\./)).toBeInTheDocument();
  });

  it("bounds the strip to three rows of height and scrolls", () => {
    const { container } = renderStrip({
      wakeups: [timeWakeup, conditionWakeup, { ...timeWakeup, id: "w3" }],
    });
    expect(screen.getAllByRole("button", { name: /^Cancel follow-up/ })).toHaveLength(3);
    const list = container.querySelector('[data-slot="chat-followup-list"]');
    expect(list).toHaveClass("max-h-[168px]", "overflow-y-auto");
  });

  it("gives Cancel a target of at least 32 px", () => {
    renderStrip();
    expect(screen.getByRole("button", { name: /^Cancel follow-up/ })).toHaveClass("min-h-8");
  });

  it("cancels the selected follow-up", () => {
    const { onCancel } = renderStrip({ wakeups: [timeWakeup, conditionWakeup] });
    fireEvent.click(screen.getAllByRole("button", { name: /^Cancel follow-up/ })[1]!);
    expect(onCancel).toHaveBeenCalledWith("w2");
  });

  it("disables Cancel natively and marks it busy while cancelling", () => {
    renderStrip({ cancellingId: "w1" });
    const cancelling = screen.getByRole("button", { name: /^Cancel follow-up/ });
    expect(cancelling).toBeDisabled();
    expect(cancelling).toHaveAttribute("aria-busy", "true");
    expect(cancelling).toHaveTextContent("Cancelling…");
    expect(cancelling).not.toHaveAttribute("aria-disabled");
  });

  it("keeps the follow-up and shows a retryable error when the cancel failed", () => {
    const { onCancel } = renderStrip({ cancelErrorId: "w1" });
    expect(screen.getByText(/Ada checks back 11:40/)).toBeInTheDocument();
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("Could not cancel the follow-up. It may still run.");
    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(onCancel).toHaveBeenCalledWith("w1");
  });

  it("disables Cancel natively offline and says why", () => {
    renderStrip({ offline: true });
    expect(screen.getByRole("button", { name: /^Cancel follow-up/ })).toBeDisabled();
    expect(screen.getByRole("status")).toHaveTextContent(
      "Offline. Reconnect to cancel; the follow-up still runs on the server.",
    );
  });

  it("shows a loading state without rows", () => {
    const { container } = renderStrip({ wakeups: [], status: "loading" });
    expect(container.querySelector('[aria-busy="true"]')).not.toBeNull();
    expect(screen.queryByRole("button", { name: /^Cancel follow-up/ })).not.toBeInTheDocument();
  });

  it("shows a retryable load error", () => {
    const { onRetry } = renderStrip({ wakeups: [], status: "error" });
    expect(screen.getByRole("alert")).toHaveTextContent("Could not load Ada's scheduled follow-ups.");
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it("cancels with the keyboard", () => {
    const { onCancel } = renderStrip();
    const button = screen.getByRole("button", { name: /^Cancel follow-up/ });
    button.focus();
    expect(button).toHaveFocus();
    fireEvent.keyDown(button, { key: "Enter" });
    fireEvent.click(button); // a native button turns Enter and Space into click
    expect(onCancel).toHaveBeenCalledWith("w1");
  });
});
