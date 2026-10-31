import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import enSettings from "../../locales/en/settings.json";

const mocks = vi.hoisted(() => ({
  desktop: false,
  support: "ok" as "ok" | "unsupported" | "needs-home-screen",
  permission: "default" as NotificationPermission | "unsupported",
  requestPermission: vi.fn(),
  config: { enabled: true, public_key: "BKey" },
  getPushConfig: vi.fn(),
  registerPushSubscription: vi.fn(),
  deletePushSubscription: vi.fn(),
  subscription: null as { endpoint: string } | null,
  subscribeThisDevice: vi.fn(),
  unsubscribeThisDevice: vi.fn(),
  setPushIntent: vi.fn(),
}));

vi.mock("../../platform", () => ({ isDesktopShell: () => mocks.desktop }));
vi.mock("../../i18n", () => ({
  useT: () => ({ t: (select: (m: typeof enSettings) => string) => select(enSettings) }),
}));
vi.mock("@multica/core/api", () => ({
  api: {
    getPushConfig: mocks.getPushConfig,
    registerPushSubscription: mocks.registerPushSubscription,
    deletePushSubscription: mocks.deletePushSubscription,
  },
}));
vi.mock("@multica/core/platform", () => ({
  getWebNotificationPermission: () => mocks.permission,
  requestWebNotificationPermission: mocks.requestPermission,
}));
vi.mock("@multica/core/push", () => ({
  getPushSupport: () => mocks.support,
  ensurePushRegistration: async () => ({}),
  getThisDeviceSubscription: async () => mocks.subscription,
  subscribeThisDevice: mocks.subscribeThisDevice,
  unsubscribeThisDevice: mocks.unsubscribeThisDevice,
  setPushIntent: mocks.setPushIntent,
}));
vi.mock("./settings-layout", () => ({
  SettingsCard: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  SettingsRow: ({ children, label, description }: { children: ReactNode; label: ReactNode; description?: ReactNode }) => (
    <div>
      <span>{label}</span>
      {description ? <span>{description}</span> : null}
      {children}
    </div>
  ),
}));

import { PushDeviceSetting } from "./push-device-setting";

function renderSetting() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <PushDeviceSetting />
    </QueryClientProvider>,
  );
}

const registration = { endpoint: "https://push.example/x", keys: { p256dh: "P", auth: "A" } };

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  mocks.desktop = false;
  mocks.support = "ok";
  mocks.permission = "default";
  mocks.config = { enabled: true, public_key: "BKey" };
  mocks.subscription = null;
  mocks.getPushConfig.mockImplementation(async () => mocks.config);
  mocks.requestPermission.mockImplementation(async () => {
    mocks.permission = "granted";
    return "granted";
  });
  mocks.subscribeThisDevice.mockResolvedValue(registration);
  mocks.registerPushSubscription.mockResolvedValue(undefined);
  mocks.unsubscribeThisDevice.mockResolvedValue("done");
});

describe("PushDeviceSetting", () => {
  it("renders nothing in the desktop shell", () => {
    mocks.desktop = true;
    const { container } = renderSetting();
    expect(container).toBeEmptyDOMElement();
    expect(mocks.getPushConfig).not.toHaveBeenCalled();
  });

  it("turns push on from the tap: permission, subscribe with the deployment key, register", async () => {
    renderSetting();
    const control = await screen.findByRole("switch", { name: "Push on this device" });
    await waitFor(() => expect(control).not.toHaveAttribute("aria-disabled","true"));
    expect(control).not.toBeChecked();
    await userEvent.click(control);
    await waitFor(() => expect(screen.getByRole("switch", { name: "Push on this device" })).toBeChecked());
    expect(mocks.requestPermission).toHaveBeenCalled();
    expect(mocks.subscribeThisDevice).toHaveBeenCalledWith(expect.anything(), "BKey");
    expect(mocks.registerPushSubscription).toHaveBeenCalledWith(registration);
    expect(mocks.setPushIntent).toHaveBeenCalledWith("on");
  });

  it("shows this device as on when the browser already holds a subscription", async () => {
    mocks.permission = "granted";
    mocks.subscription = { endpoint: "https://push.example/x" };
    renderSetting();
    await waitFor(() => expect(screen.getByRole("switch", { name: "Push on this device" })).toBeChecked());
  });

  it("turns push off by removing the device", async () => {
    mocks.permission = "granted";
    mocks.subscription = { endpoint: "https://push.example/x" };
    renderSetting();
    const control = await screen.findByRole("switch", { name: "Push on this device" });
    await waitFor(() => expect(control).toBeChecked());
    mocks.unsubscribeThisDevice.mockImplementationOnce(async () => {
      mocks.subscription = null;
      return "done";
    });
    await userEvent.click(control);
    await waitFor(() => expect(mocks.unsubscribeThisDevice).toHaveBeenCalledWith(expect.objectContaining({ deletePushSubscription: expect.any(Function) })));
    await waitFor(() => expect(screen.getByRole("switch", { name: "Push on this device" })).not.toBeChecked());
  });

  it("says so when turning push off fails, keeps the switch on, and recovers on retry", async () => {
    mocks.permission = "granted";
    mocks.subscription = { endpoint: "https://push.example/x" };
    mocks.unsubscribeThisDevice.mockResolvedValueOnce("failed");
    renderSetting();
    const control = await screen.findByRole("switch", { name: "Push on this device" });
    await waitFor(() => expect(control).toBeChecked());
    await userEvent.click(control);
    expect(await screen.findByText(enSettings.notifications.device.disable_failed)).toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Push on this device" })).toBeChecked();

    mocks.unsubscribeThisDevice.mockImplementationOnce(async () => {
      mocks.subscription = null;
      return "done";
    });
    await userEvent.click(screen.getByRole("switch", { name: "Push on this device" }));
    await waitFor(() => expect(screen.getByRole("switch", { name: "Push on this device" })).not.toBeChecked());
    expect(screen.queryByText(enSettings.notifications.device.disable_failed)).not.toBeInTheDocument();
  });

  it("explains a blocked browser and keeps the switch off", async () => {
    mocks.permission = "denied";
    renderSetting();
    expect(await screen.findByText(enSettings.notifications.device.blocked)).toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Push on this device" })).toHaveAttribute("aria-disabled","true");
  });

  it("stays off when the user dismisses the permission prompt", async () => {
    mocks.requestPermission.mockResolvedValue("default");
    renderSetting();
    const control = await screen.findByRole("switch", { name: "Push on this device" });
    await waitFor(() => expect(control).not.toHaveAttribute("aria-disabled","true"));
    await userEvent.click(control);
    await waitFor(() => expect(mocks.requestPermission).toHaveBeenCalled());
    expect(mocks.registerPushSubscription).not.toHaveBeenCalled();
    expect(screen.getByRole("switch", { name: "Push on this device" })).not.toBeChecked();
  });

  it("says so when registration fails and leaves the switch off", async () => {
    mocks.registerPushSubscription.mockRejectedValue(new Error("503"));
    renderSetting();
    const control = await screen.findByRole("switch", { name: "Push on this device" });
    await waitFor(() => expect(control).not.toHaveAttribute("aria-disabled","true"));
    await userEvent.click(control);
    expect(await screen.findByText(enSettings.notifications.device.failed)).toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Push on this device" })).not.toBeChecked();
  });

  it("keeps the requested intent when registration fails, so a reload retries it", async () => {
    mocks.registerPushSubscription.mockRejectedValue(new Error("503"));
    renderSetting();
    const control = await screen.findByRole("switch", { name: "Push on this device" });
    await waitFor(() => expect(control).not.toHaveAttribute("aria-disabled","true"));
    await userEvent.click(control);
    expect(await screen.findByText(enSettings.notifications.device.failed)).toBeInTheDocument();
    expect(mocks.setPushIntent).toHaveBeenCalledWith("on");
    expect(mocks.setPushIntent).not.toHaveBeenCalledWith("off");
    // Recorded before the request that can fail, not after it.
    expect(mocks.setPushIntent.mock.invocationCallOrder[0]!).toBeLessThan(
      mocks.registerPushSubscription.mock.invocationCallOrder[0]!,
    );
  });

  it("records no intent when the permission prompt is declined", async () => {
    mocks.requestPermission.mockImplementation(async () => {
      mocks.permission = "denied";
      return "denied";
    });
    renderSetting();
    const control = await screen.findByRole("switch", { name: "Push on this device" });
    await waitFor(() => expect(control).not.toHaveAttribute("aria-disabled","true"));
    await userEvent.click(control);
    await waitFor(() => expect(mocks.requestPermission).toHaveBeenCalled());
    expect(mocks.setPushIntent).not.toHaveBeenCalled();
  });

  it("says push is not set up when the server has no keys", async () => {
    mocks.config = { enabled: false, public_key: "" };
    renderSetting();
    expect(await screen.findByText(enSettings.notifications.device.not_configured)).toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Push on this device" })).toHaveAttribute("aria-disabled","true");
  });

  it("says the browser cannot receive push", async () => {
    mocks.support = "unsupported";
    renderSetting();
    expect(await screen.findByText(enSettings.notifications.device.unsupported)).toBeInTheDocument();
    expect(screen.queryByRole("switch", { name: "Push on this device" })).not.toBeInTheDocument();
  });

  it("replaces the switch with Home Screen steps on iPhone Safari", async () => {
    mocks.support = "needs-home-screen";
    renderSetting();
    expect(await screen.findByText(enSettings.notifications.device.ios.title)).toBeInTheDocument();
    expect(screen.getByText(enSettings.notifications.device.ios.step_add)).toBeInTheDocument();
    expect(screen.getByText(enSettings.notifications.device.ios.step_sign_in)).toBeInTheDocument();
    expect(screen.queryByRole("switch")).not.toBeInTheDocument();
  });
});
