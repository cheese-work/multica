import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import { describe, expect, it, vi } from "vitest";
import enChat from "../../locales/en/chat.json";
import { ChatQueue } from "./chat-queue";

const TEST_RESOURCES = { en: { chat: enChat } };

function renderQueue(
  headStatus = "running",
  sendNowDisabled = false,
  headSupplement: "capable" | "starting" | "unsupported" | "absent" = "capable",
) {
  const callbacks = {
    onSteer: vi.fn<(taskId: string) => Promise<void>>().mockResolvedValue(),
    onStopAndSend: vi.fn<(taskId: string) => Promise<void>>().mockResolvedValue(),
    onEdit: vi.fn<(taskId: string) => Promise<void>>().mockResolvedValue(),
    onRemove: vi.fn<(taskId: string) => Promise<void>>().mockResolvedValue(),
    onClear: vi.fn<() => Promise<void>>().mockResolvedValue(),
  };
  const view = render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <ChatQueue
        headStatus={headStatus}
        headSupplement={headSupplement === "absent" ? undefined : headSupplement}
        sendNowDisabled={sendNowDisabled}
        tasks={[
          {
            task_id: "task-2",
            status: "queued",
            content: "First follow-up",
            created_at: "2026-07-01T00:01:00Z",
          },
          {
            task_id: "task-3",
            status: "queued",
            content: "",
            created_at: "2026-07-01T00:02:00Z",
          },
        ]}
        {...callbacks}
      />
    </I18nProvider>,
  );
  return { ...callbacks, container: view.container };
}

describe("ChatQueue", () => {
  it("renders a standalone queue card without a separate header", () => {
    const { container } = renderQueue();

    expect(screen.getByRole("region", { name: "2 queued messages" })).toBeInTheDocument();
    expect(screen.queryByText("2 queued messages")).not.toBeInTheDocument();
    expect(screen.getByText("First follow-up")).toBeInTheDocument();
    expect(screen.getByText("Queued message")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "Steer" })).toHaveLength(2);
    expect(screen.getAllByLabelText("Remove queued message")).toHaveLength(2);
    expect(screen.getAllByLabelText("More queue actions")).toHaveLength(2);
    expect(screen.queryByRole("button", { name: "Clear all" })).not.toBeInTheDocument();

    const shell = container.querySelector('[data-slot="chat-queue-shell"]');
    const queue = container.querySelector('[data-slot="chat-queue"]');
    // Tucked stack, owned entirely by the queue: it slides under the composer
    // (negative margin + z-0) while the composer's chrome stays untouched.
    expect(shell).toHaveClass("z-0", "-mb-3");
    expect(queue).toHaveClass(
      "rounded-lg",
      "border-surface-border",
      "bg-surface",
      "pb-4",
    );
    expect(container.querySelectorAll('[data-slot="chat-queue-row"]')).toHaveLength(2);
    expect(container.querySelectorAll('[data-slot="chat-queue-item-icon"]')).toHaveLength(2);
  });

  it("runs steer, remove, and overflow actions against the selected queue state", async () => {
    const actions = renderQueue();

    fireEvent.click(screen.getAllByRole("button", { name: "Steer" })[1]!);
    await waitFor(() => expect(actions.onSteer).toHaveBeenCalledWith("task-3"));
    expect(actions.onStopAndSend).not.toHaveBeenCalled();

    fireEvent.click(screen.getAllByLabelText("More queue actions")[0]!);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Edit queued message" }));
    await waitFor(() => expect(actions.onEdit).toHaveBeenCalledWith("task-2"));

    fireEvent.click(screen.getAllByLabelText("Remove queued message")[1]!);
    await waitFor(() => expect(actions.onRemove).toHaveBeenCalledWith("task-3"));

    fireEvent.click(screen.getAllByLabelText("More queue actions")[1]!);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Clear all" }));
    await waitFor(() => expect(actions.onClear).toHaveBeenCalledTimes(1));
  });

  it("disables send-now until the current positional head is claimable", () => {
    const actions = renderQueue("queued");

    const buttons = screen.getAllByRole("button", {
      name: "Steer is available after the current reply starts",
    });
    expect(buttons).toHaveLength(2);
    for (const button of buttons) expect(button).toBeDisabled();
    expect(actions.onSteer).not.toHaveBeenCalled();
  });

  it("keeps long queues bounded and blocks duplicate actions while one is pending", async () => {
    let finishClear: (() => void) | undefined;
    const actions = renderQueue();
    actions.onClear.mockReturnValue(new Promise<void>((resolve) => {
      finishClear = resolve;
    }));

    const scroller = actions.container.querySelector('[data-slot="chat-queue-list"]');
    expect(scroller).toHaveClass("max-h-40");

    const clearTrigger = screen.getAllByLabelText("More queue actions")[0]!;
    fireEvent.click(clearTrigger);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Clear all" }));
    await waitFor(() => {
      expect(clearTrigger.querySelector(".animate-spin")).toBeInTheDocument();
      for (const button of screen.getAllByRole("button")) {
        expect(button).toBeDisabled();
      }
    });

    finishClear?.();
    await waitFor(() => {
      expect(clearTrigger.querySelector(".animate-spin")).not.toBeInTheDocument();
      for (const button of screen.getAllByRole("button")) {
        expect(button).toBeEnabled();
      }
    });
    expect(actions.onClear).toHaveBeenCalledTimes(1);
  });
});

// MUL-6380: steering a queued message dispatches it now, so it has to clear the
// same invoke gate as a fresh send. When the caller has lost permission to run
// the agent, a live-looking Steer button just walks them into a 403.
describe("ChatQueue send-now gating", () => {
  it("blocks Steer when the caller may no longer invoke the agent", async () => {
    const { onSteer, onStopAndSend } = renderQueue("running", true);

    // The label must state the real reason: "wait for the reply to start" would
    // send the user waiting for something waiting cannot fix.
    const steer = screen.getAllByRole("button", {
      name: "You no longer have permission to run this agent",
    })[0]!;
    expect(steer).toBeDisabled();
    fireEvent.click(steer);
    await waitFor(() => expect(onSteer).not.toHaveBeenCalled());
    expect(onStopAndSend).not.toHaveBeenCalled();
  });

  it("leaves Steer available when the head task is dispatchable and permitted", () => {
    renderQueue("running", false);

    expect(screen.getAllByRole("button", { name: "Steer" })[0]!).not.toBeDisabled();
  });
});

// CHE-2820: Steer adds the message to the running reply; stopping the run is a
// separate, explicit action.
describe("ChatQueue Steer and Stop & send", () => {
  it("keeps Stop & send as a separate action that stops the run", async () => {
    const actions = renderQueue("running", false, "capable");

    fireEvent.click(screen.getAllByLabelText("More queue actions")[1]!);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Stop & send" }));
    await waitFor(() => expect(actions.onStopAndSend).toHaveBeenCalledWith("task-3"));
    expect(actions.onSteer).not.toHaveBeenCalled();
  });

  it("disables Steer with the reason while the run is starting, never as unsupported", () => {
    renderQueue("running", false, "starting");

    const steer = screen.getAllByRole("button", { name: "Run is starting" })[0]!;
    expect(steer).toBeDisabled();
    expect(screen.queryByRole("button", { name: /can't take new messages/ })).not.toBeInTheDocument();
  });

  it.each(["unsupported", "absent"] as const)(
    "disables Steer with the queue reason when head support is %s",
    (support) => {
      renderQueue("running", false, support);

      const steer = screen.getAllByRole("button", {
        name: "This run can't take new messages. The message stays queued.",
      })[0]!;
      expect(steer).toBeDisabled();
    },
  );

  it("still offers Stop & send when the run cannot be steered", async () => {
    const actions = renderQueue("running", false, "unsupported");

    fireEvent.click(screen.getAllByLabelText("More queue actions")[0]!);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Stop & send" }));
    await waitFor(() => expect(actions.onStopAndSend).toHaveBeenCalledWith("task-2"));
  });

  it.each([
    ["running", "starting", "Run is starting"],
    ["running", "unsupported", "This run can't take new messages. The message stays queued."],
    ["running", "absent", "This run can't take new messages. The message stays queued."],
    ["queued", "capable", "Steer is available after the current reply starts"],
  ] as const)(
    "shows the fallback reason as visible text (head %s, support %s)",
    (headStatus, support, reason) => {
      renderQueue(headStatus, false, support);

      const visible = screen.getByText(reason);
      expect(visible).toBeVisible();
      expect(visible).toHaveAttribute("data-slot", "chat-queue-reason");
    },
  );

  it("shows no reason text while Steer is available", () => {
    const { container } = renderQueue("running", false, "capable");

    expect(container.querySelector('[data-slot="chat-queue-reason"]')).toBeNull();
  });
});
