import { beforeEach, describe, expect, it, vi, type Mock } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import { useArchivedSend } from "./use-archived-send";

const archivedSession = {
  id: "old",
  status: "archived",
  title: "Old onboarding notes",
  continued_into_session_id: null,
} as ChatSession;
const continuedSession = {
  ...archivedSession,
  id: "dash",
  title: "Dashboard rollout",
  continued_into_session_id: "dash-2",
} as ChatSession;
const newerSession = { id: "dash-2", status: "active", title: "Dashboard rollout (2)" } as ChatSession;
const activeSession = { id: "live", status: "active", title: "Live" } as ChatSession;

type Harness = {
  session: ChatSession;
  sessions?: ChatSession[];
  send: Mock<(...args: unknown[]) => Promise<boolean>>;
  unarchive: Mock<(session: ChatSession) => Promise<boolean>>;
  onOpenSession: Mock<(session: ChatSession) => void>;
};

function Probe({ session, sessions = [], send, unarchive, onOpenSession }: Harness) {
  const { guardedSend, dialog } = useArchivedSend({ session, sessions, send, unarchive, onOpenSession });
  return (
    <>
      <button
        type="button"
        onClick={() => void guardedSend("hello", ["att-1"], vi.fn())}
      >
        send
      </button>
      {dialog}
    </>
  );
}

function setup(over: Partial<Harness> & { session: ChatSession }) {
  const props: Harness = {
    send: vi.fn(async () => true),
    unarchive: vi.fn(async () => true),
    onOpenSession: vi.fn(),
    ...over,
  };
  render(
    <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
      <Probe {...props} />
    </I18nProvider>,
  );
  return props;
}

beforeEach(() => vi.clearAllMocks());

describe("useArchivedSend", () => {
  it("passes a send on an active DM straight through", () => {
    const { send, unarchive } = setup({ session: activeSession });
    fireEvent.click(screen.getByText("send"));
    expect(send).toHaveBeenCalledWith("hello", ["att-1"], expect.any(Function));
    expect(unarchive).not.toHaveBeenCalled();
    expect(screen.queryByTestId("chat-archived-send-dialog")).not.toBeInTheDocument();
  });

  it("asks before sending from an archived DM and sends nothing yet", async () => {
    const { send, unarchive } = setup({ session: archivedSession });
    fireEvent.click(screen.getByText("send"));

    const dialog = await screen.findByTestId("chat-archived-send-dialog");
    expect(dialog).toHaveTextContent("This DM is archived");
    expect(dialog).toHaveTextContent("Unarchive it and send?");
    expect(send).not.toHaveBeenCalled();
    expect(unarchive).not.toHaveBeenCalled();
  });

  it("unarchives first and then sends exactly once with the original message", async () => {
    const order: string[] = [];
    const unarchive = vi.fn(async () => {
      order.push("unarchive");
      return true;
    });
    const send = vi.fn(async () => {
      order.push("send");
      return true;
    });
    setup({ session: archivedSession, send, unarchive });
    fireEvent.click(screen.getByText("send"));
    const confirm = await screen.findByRole("button", { name: "Unarchive and send" });

    fireEvent.click(confirm);
    fireEvent.click(confirm);

    await waitFor(() => expect(send).toHaveBeenCalledTimes(1));
    expect(unarchive).toHaveBeenCalledTimes(1);
    expect(unarchive).toHaveBeenCalledWith(archivedSession);
    expect(order).toEqual(["unarchive", "send"]);
    expect(send).toHaveBeenCalledWith("hello", ["att-1"], expect.any(Function));
    await waitFor(() => expect(screen.queryByTestId("chat-archived-send-dialog")).not.toBeInTheDocument());
  });

  it("changes nothing when the user cancels", async () => {
    const { send, unarchive } = setup({ session: archivedSession });
    fireEvent.click(screen.getByText("send"));

    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));

    await waitFor(() => expect(screen.queryByTestId("chat-archived-send-dialog")).not.toBeInTheDocument());
    expect(send).not.toHaveBeenCalled();
    expect(unarchive).not.toHaveBeenCalled();
  });

  it("sends nothing when the unarchive fails", async () => {
    const unarchive = vi.fn(async () => false);
    const { send } = setup({ session: archivedSession, unarchive });
    fireEvent.click(screen.getByText("send"));

    fireEvent.click(await screen.findByRole("button", { name: "Unarchive and send" }));

    await waitFor(() => expect(unarchive).toHaveBeenCalledTimes(1));
    expect(send).not.toHaveBeenCalled();
    await waitFor(() => expect(screen.queryByTestId("chat-archived-send-dialog")).not.toBeInTheDocument());
  });

  it("makes Open the newer DM the primary action for a continued DM", async () => {
    const { send, unarchive, onOpenSession } = setup({
      session: continuedSession,
      sessions: [continuedSession, newerSession],
    });
    fireEvent.click(screen.getByText("send"));

    const dialog = await screen.findByTestId("chat-archived-send-dialog");
    expect(dialog).toHaveTextContent("A newer DM continues this conversation: Dashboard rollout (2).");
    const open = screen.getByRole("button", { name: "Open the newer DM" });
    const secondary = screen.getByRole("button", { name: "Unarchive and send" });
    expect(open).toHaveAttribute("data-primary", "true");
    expect(secondary).not.toHaveAttribute("data-primary");
    expect(screen.getByRole("button", { name: "Cancel" })).toBeInTheDocument();

    fireEvent.click(open);

    expect(onOpenSession).toHaveBeenCalledWith(newerSession);
    expect(send).not.toHaveBeenCalled();
    expect(unarchive).not.toHaveBeenCalled();
  });

  it("offers no newer DM when the DM was not continued", async () => {
    setup({ session: archivedSession });
    fireEvent.click(screen.getByText("send"));
    await screen.findByTestId("chat-archived-send-dialog");
    expect(screen.queryByRole("button", { name: "Open the newer DM" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Unarchive and send" })).toHaveAttribute("data-primary", "true");
  });

  it("still offers unarchive and send when the newer DM is not in the list", async () => {
    setup({ session: continuedSession, sessions: [continuedSession] });
    fireEvent.click(screen.getByText("send"));
    await screen.findByTestId("chat-archived-send-dialog");
    expect(screen.queryByRole("button", { name: "Open the newer DM" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Unarchive and send" })).toBeInTheDocument();
  });
});
