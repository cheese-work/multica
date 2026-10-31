import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, waitFor } from "@testing-library/react";

const state = vi.hoisted(() => ({
  desktop: false,
  permission: "granted" as string,
  channels: { inbox: { in_app: true, push: false }, dm: { in_app: false, push: false } },
  swListeners: [] as ((e: { data: unknown }) => void)[],
}));
const reRegister = vi.hoisted(() => vi.fn(async () => "registered"));
const resubscribe = vi.hoisted(() => vi.fn(async () => "registered"));
const toastWarning = vi.hoisted(() => vi.fn());

vi.mock("../../platform", () => ({ isDesktopShell: () => state.desktop }));
vi.mock("@multica/core/push", () => ({
  reRegisterThisDevice: reRegister,
  resubscribeThisDevice: resubscribe,
}));
vi.mock("@multica/core/api", () => ({ api: {} }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws" }));
vi.mock("@multica/core/platform", () => ({ getWebNotificationPermission: () => state.permission }));
vi.mock("@multica/core/notification-preferences/channels", () => ({
  notificationChannelOptions: () => ({ queryKey: ["notification-channels", "ws"] }),
}));
vi.mock("@tanstack/react-query", () => ({
  useQuery: () => ({ data: { channels: state.channels } }),
}));
vi.mock("../../i18n", () => ({ useT: () => ({ t: () => "Notifications are blocked in this browser" }) }));
vi.mock("sonner", () => ({ toast: { warning: toastWarning } }));

import { PushSync } from "./push-sync";

describe("PushSync", () => {
  beforeEach(() => {
    state.desktop = false;
    state.permission = "granted";
    state.channels = { inbox: { in_app: true, push: false }, dm: { in_app: false, push: false } };
    state.swListeners = [];
    vi.stubGlobal("navigator", {
      serviceWorker: {
        addEventListener: (_: string, fn: (e: { data: unknown }) => void) => state.swListeners.push(fn),
        removeEventListener: (_: string, fn: (e: { data: unknown }) => void) => {
          state.swListeners = state.swListeners.filter((l) => l !== fn);
        },
      },
    });
  });
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    vi.clearAllMocks();
  });

  it("re-registers this browser's subscription on load", async () => {
    render(<PushSync />);
    await waitFor(() => expect(reRegister).toHaveBeenCalledTimes(1));
  });

  it("re-subscribes when the service worker reports a changed subscription", async () => {
    render(<PushSync />);
    state.swListeners.forEach((l) => l({ data: { type: "push-subscription-changed" } }));
    state.swListeners.forEach((l) => l({ data: { type: "something-else" } }));
    await waitFor(() => expect(resubscribe).toHaveBeenCalledTimes(1));
  });

  it("stops listening on unmount", () => {
    const { unmount } = render(<PushSync />);
    unmount();
    expect(state.swListeners).toHaveLength(0);
  });

  it("does nothing in the desktop shell", async () => {
    state.desktop = true;
    render(<PushSync />);
    expect(reRegister).not.toHaveBeenCalled();
    expect(state.swListeners).toHaveLength(0);
    expect(toastWarning).not.toHaveBeenCalled();
  });

  it("warns once when a Push switch is on and the browser blocks notifications", async () => {
    state.permission = "denied";
    state.channels.dm.push = true;
    render(<PushSync />);
    await waitFor(() => expect(toastWarning).toHaveBeenCalledTimes(1));
    expect(toastWarning).toHaveBeenCalledWith(
      "Notifications are blocked in this browser",
      expect.objectContaining({ id: "push-permission-blocked" }),
    );
  });

  it("stays quiet when blocked but no Push switch is on", async () => {
    state.permission = "denied";
    render(<PushSync />);
    await waitFor(() => expect(reRegister).toHaveBeenCalled());
    expect(toastWarning).not.toHaveBeenCalled();
  });

  it("stays quiet when permission is granted", async () => {
    state.channels.inbox.push = true;
    render(<PushSync />);
    await waitFor(() => expect(reRegister).toHaveBeenCalled());
    expect(toastWarning).not.toHaveBeenCalled();
  });
});
