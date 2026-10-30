import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import { createChatStore, registerChatStore } from "@multica/core/chat";
import type { ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";

const archiveAsync = vi.hoisted(() => vi.fn());
const pinAsync = vi.hoisted(() => vi.fn());
const toastFn = vi.hoisted(() => Object.assign(vi.fn(), { error: vi.fn() }));

vi.mock("sonner", () => ({ toast: toastFn }));
vi.mock("@multica/core/chat/mutations", () => ({
  useSetChatSessionArchived: () => ({ mutateAsync: archiveAsync }),
  useSetChatSessionPinned: () => ({ mutateAsync: pinAsync }),
}));

import { useChatSessionActions } from "./use-chat-session-actions";

const session = { id: "s1", status: "active", pinned: false, title: "Plan" } as ChatSession;
const archived = { ...session, status: "archived" } as ChatSession;

function setup() {
  return renderHook(() => useChatSessionActions(), {
    wrapper: ({ children }) => (
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
        {children}
      </I18nProvider>
    ),
  });
}

beforeEach(() => {
  archiveAsync.mockReset();
  pinAsync.mockReset();
  toastFn.mockReset();
  toastFn.error.mockReset();
  registerChatStore(createChatStore({ storage: localStorage }));
});

describe("useChatSessionActions", () => {
  it("navigates only after the server accepted the archive, then offers a 5 s undo", async () => {
    const order: string[] = [];
    archiveAsync.mockImplementation(async () => {
      order.push("server");
    });
    const onArchived = vi.fn(() => order.push("navigate"));
    const { result } = setup();

    let ok = false;
    await act(async () => {
      ok = await result.current.archive(session, { onArchived });
    });

    expect(ok).toBe(true);
    expect(archiveAsync).toHaveBeenCalledWith({ sessionId: "s1", archived: true });
    expect(order).toEqual(["server", "navigate"]);
    expect(toastFn).toHaveBeenCalledWith(
      "Chat archived",
      expect.objectContaining({ duration: 5000, action: expect.objectContaining({ label: "Undo" }) }),
    );
  });

  it("keeps the session open and shows the server error when the archive fails", async () => {
    archiveAsync.mockRejectedValue(new Error("This DM is in a decision lane"));
    const onArchived = vi.fn();
    const { result } = setup();

    let ok = true;
    await act(async () => {
      ok = await result.current.archive(session, { onArchived });
    });

    expect(ok).toBe(false);
    expect(onArchived).not.toHaveBeenCalled();
    expect(toastFn.error).toHaveBeenCalledWith("This DM is in a decision lane");
    expect(toastFn).not.toHaveBeenCalled();
  });

  it("undoes an archive with an unarchive", async () => {
    archiveAsync.mockResolvedValue(undefined);
    const { result } = setup();
    await act(async () => {
      await result.current.archive(session);
    });
    const options = toastFn.mock.calls[0]?.[1] as { action: { onClick: () => void } };

    await act(async () => {
      options.action.onClick();
    });

    expect(archiveAsync).toHaveBeenLastCalledWith({ sessionId: "s1", archived: false });
  });

  it("ignores a second archive while the first is in flight", async () => {
    let finish: () => void = () => {};
    archiveAsync.mockReturnValue(new Promise<void>((resolve) => (finish = resolve)));
    const { result } = setup();

    let first: Promise<boolean> = Promise.resolve(false);
    let second = true;
    await act(async () => {
      first = result.current.archive(session);
      second = await result.current.archive(session);
    });
    expect(second).toBe(false);
    expect(archiveAsync).toHaveBeenCalledTimes(1);

    await act(async () => {
      finish();
      await first;
    });
    await act(async () => {
      await result.current.archive(session);
    });
    expect(archiveAsync).toHaveBeenCalledTimes(2);
  });

  it("unarchives through the same guard and reports a failure", async () => {
    archiveAsync.mockRejectedValue(new Error("nope"));
    const { result } = setup();

    let ok = true;
    await act(async () => {
      ok = await result.current.unarchive(archived);
    });

    expect(ok).toBe(false);
    expect(archiveAsync).toHaveBeenCalledWith({ sessionId: "s1", archived: false });
    expect(toastFn.error).toHaveBeenCalledWith("nope");
  });

  it("toggles the pin of an active DM and refuses an archived one", async () => {
    pinAsync.mockResolvedValue(undefined);
    const { result } = setup();

    await act(async () => {
      expect(await result.current.togglePin(session)).toBe(true);
    });
    expect(pinAsync).toHaveBeenCalledWith({ sessionId: "s1", pinned: true });

    await act(async () => {
      expect(await result.current.togglePin({ ...session, pinned: true } as ChatSession)).toBe(true);
    });
    expect(pinAsync).toHaveBeenLastCalledWith({ sessionId: "s1", pinned: false });

    pinAsync.mockClear();
    await act(async () => {
      expect(await result.current.togglePin(archived)).toBe(false);
    });
    expect(pinAsync).not.toHaveBeenCalled();
  });

  it("shows the server error when pinning fails", async () => {
    pinAsync.mockRejectedValue(new Error("pin refused"));
    const { result } = setup();
    await act(async () => {
      expect(await result.current.togglePin(session)).toBe(false);
    });
    expect(toastFn.error).toHaveBeenCalledWith("pin refused");
  });
});
