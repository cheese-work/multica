import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, renderHook, screen, waitFor } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import { createChatStore, registerChatStore } from "@multica/core/chat";
import type { ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";

const archiveAsync = vi.hoisted(() => vi.fn());
const pinAsync = vi.hoisted(() => vi.fn());
const toastError = vi.hoisted(() => vi.fn());

vi.mock("sonner", () => ({ toast: Object.assign(vi.fn(), { error: toastError }) }));
vi.mock("@multica/core/chat/mutations", () => ({
  useSetChatSessionArchived: () => ({ mutateAsync: archiveAsync }),
  useSetChatSessionPinned: () => ({ mutateAsync: pinAsync }),
}));
vi.mock("./chat-session-stats-dialog", () => ({
  ChatSessionStatsDialog: ({ open, sessionId }: { open: boolean; sessionId: string | null }) =>
    open ? <div data-testid="stats-open">{sessionId}</div> : null,
}));

import { useDmSessionSurface } from "./use-dm-session-surface";

const active = { id: "s1", status: "active", pinned: false, title: "Plan" } as ChatSession;
const archived = { ...active, status: "archived" } as ChatSession;

function setup(session: ChatSession | null) {
  const props = {
    send: vi.fn(async () => true),
    onOpenSession: vi.fn(),
    onArchived: vi.fn(),
    onRename: vi.fn(async () => true),
    onFork: vi.fn(),
  };
  const view = renderHook(
    () =>
      useDmSessionSurface({
        session,
        sessions: session ? [session] : [],
        ...props,
      }),
    {
      wrapper: ({ children }) => (
        <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
          {children}
        </I18nProvider>
      ),
    },
  );
  return { ...props, ...view };
}

beforeEach(() => {
  vi.clearAllMocks();
  archiveAsync.mockReset();
  pinAsync.mockReset();
  registerChatStore(createChatStore({ storage: localStorage }));
});

describe("useDmSessionSurface", () => {
  it("wires statistic, archive, pin, rename and fork for an active DM", () => {
    const { result, onRename, onFork } = setup(active);
    const input = result.current.inputProps;
    expect(input.onStatistic).toBeTypeOf("function");
    expect(input.onArchive).toBeTypeOf("function");
    expect(input.onPin).toBeTypeOf("function");
    expect(input.onUnarchive).toBeUndefined();
    expect(input.onRename).toBe(onRename);
    expect(input.onFork).toBe(onFork);
    expect(input.sessionArchived).toBe(false);
  });

  it("wires only unarchive for an archived DM and drops rename and fork", () => {
    const { result } = setup(archived);
    const input = result.current.inputProps;
    expect(input.onUnarchive).toBeTypeOf("function");
    expect(input.onStatistic).toBeUndefined();
    expect(input.onArchive).toBeUndefined();
    expect(input.onPin).toBeUndefined();
    expect(input.onRename).toBeUndefined();
    expect(input.onFork).toBeUndefined();
    expect(input.sessionArchived).toBe(true);
  });

  it("wires nothing for a chat that does not exist yet", () => {
    const { result } = setup(null);
    const input = result.current.inputProps;
    expect(input.onStatistic).toBeUndefined();
    expect(input.onArchive).toBeUndefined();
    expect(input.onUnarchive).toBeUndefined();
  });

  it("moves the pane off the DM only after the server archived it", async () => {
    const order: string[] = [];
    archiveAsync.mockImplementation(async () => void order.push("server"));
    const { result, onArchived } = setup(active);
    onArchived.mockImplementation(() => void order.push("navigate"));

    await act(async () => {
      await result.current.inputProps.onArchive?.();
    });

    expect(order).toEqual(["server", "navigate"]);
    expect(onArchived).toHaveBeenCalledWith(active);
  });

  it("keeps the DM open and reports the server error when the archive fails", async () => {
    archiveAsync.mockRejectedValue(new Error("decision lane"));
    const { result, onArchived } = setup(active);

    let ok: boolean | void = true;
    await act(async () => {
      ok = await result.current.inputProps.onArchive?.();
    });

    expect(ok).toBe(false);
    expect(onArchived).not.toHaveBeenCalled();
    expect(toastError).toHaveBeenCalledWith("decision lane");
  });

  it("opens the statistics dialog for /statistic and sends nothing", () => {
    const { result, send } = setup(active);
    act(() => result.current.inputProps.onStatistic?.());
    render(<>{result.current.dialogs}</>);
    expect(screen.getByTestId("stats-open")).toHaveTextContent("s1");
    expect(send).not.toHaveBeenCalled();
  });

  it("holds an archived send until confirmed, then unarchives and sends once", async () => {
    archiveAsync.mockResolvedValue(undefined);
    const { result, send } = setup(archived);

    act(() => void result.current.inputProps.onSend("hi", undefined, vi.fn(), []));
    expect(send).not.toHaveBeenCalled();

    render(
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
        {result.current.dialogs}
      </I18nProvider>,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Unarchive and send" }));

    await waitFor(() => expect(send).toHaveBeenCalledTimes(1));
    expect(archiveAsync).toHaveBeenCalledWith({ sessionId: "s1", archived: false });
  });
});
