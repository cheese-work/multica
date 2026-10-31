import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, renderHook, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { NotificationPreferences } from "@multica/core/types";
import enSettings from "../../locales/en/settings.json";

const state = vi.hoisted(() => ({
  preferences: {} as NotificationPreferences,
  mutate: vi.fn(),
  channels: {
    inbox: { in_app: true, push: false },
    dm: { in_app: false, push: false },
  } as Record<"inbox" | "dm", { in_app: boolean; push: boolean }>,
  mutateChannel: vi.fn(),
  desktop: false,
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace" }));
vi.mock("@tanstack/react-query", () => ({
  queryOptions: (options: unknown) => options,
  useQuery: (options: { queryKey: readonly unknown[] }) =>
    options.queryKey[0] === "notification-channels"
      ? { data: { channels: state.channels } }
      : { data: { preferences: state.preferences } },
}));
vi.mock("@multica/core/notification-preferences/channels", () => ({
  notificationChannelOptions: () => ({ queryKey: ["notification-channels", "workspace"] }),
  useUpdateNotificationChannel: () => ({ mutate: state.mutateChannel }),
}));
vi.mock("../../platform", () => ({ isDesktopShell: () => state.desktop }));
vi.mock("./push-device-setting", () => ({ PushDeviceSetting: () => <div>push device</div> }));
vi.mock("@multica/core/notification-preferences/mutations", () => ({
  useUpdateNotificationPreferences: () => ({ mutate: state.mutate }),
}));
vi.mock("../../i18n", () => ({
  useT: () => ({ t: (select: (messages: typeof enSettings) => string) => select(enSettings) }),
}));
vi.mock("./browser-notification-setting", () => ({ BrowserNotificationSetting: () => null }));
vi.mock("./settings-layout", () => ({
  SettingsTab: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  SettingsSection: ({ children, title }: { children: ReactNode; title: string }) => <section><h2>{title}</h2>{children}</section>,
  SettingsCard: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  SettingsRow: ({ children, label }: { children: ReactNode; label: string }) => <div>{label}{children}</div>,
}));

import { NotificationsTab } from "./notifications-tab";
import { useSettingsSearchIndex } from "./use-settings-search-index";

describe("notification delivery scope", () => {
  beforeEach(() => {
    cleanup();
    state.preferences = {};
    state.channels = { inbox: { in_app: true, push: false }, dm: { in_app: false, push: false } };
    state.desktop = false;
    vi.clearAllMocks();
  });

  it("makes the delivery scope discoverable in settings search", () => {
    const { result } = renderHook(() => useSettingsSearchIndex([{ value: "notifications", label: "Notifications" }]));
    expect(result.current).toContainEqual({
      tab: "notifications",
      anchor: "unassigned_activity",
      title: "Only decisions and issues assigned to me",
    });
  });

  it("defaults to all activity and preserves unrelated mutes when narrowing", async () => {
    state.preferences = { comments: "muted", system_notifications: "muted" };
    render(<NotificationsTab />);
    const control = screen.getByRole("switch", { name: "Only decisions and issues assigned to me" });
    expect(control).not.toBeChecked();
    await userEvent.click(control);
    expect(state.mutate).toHaveBeenCalledWith(
      { comments: "muted", system_notifications: "muted", unassigned_activity: "muted" },
      expect.any(Object),
    );
  });

  it("restores all activity without changing event controls", async () => {
    state.preferences = { unassigned_activity: "muted", mentions: "muted" };
    render(<NotificationsTab />);
    const control = screen.getByRole("switch", { name: "Only decisions and issues assigned to me" });
    expect(control).toBeChecked();
    await userEvent.click(control);
    expect(state.mutate).toHaveBeenCalledWith({ mentions: "muted" }, expect.any(Object));
    expect(screen.getByRole("switch", { name: "Mentions" })).not.toBeChecked();
  });
});

describe("channel switches", () => {
  beforeEach(() => {
    cleanup();
    state.channels = { inbox: { in_app: true, push: false }, dm: { in_app: false, push: false } };
    state.desktop = false;
    vi.clearAllMocks();
  });

  it("shows the CHE-1848 defaults: Inbox in-app on, everything else off", () => {
    render(<NotificationsTab />);
    const inApp = screen.getAllByRole("switch", { name: "In-app" });
    const push = screen.getAllByRole("switch", { name: "Push" });
    expect(inApp.map((el) => el.getAttribute("aria-checked"))).toEqual(["true", "false"]);
    expect(push.map((el) => el.getAttribute("aria-checked"))).toEqual(["false", "false"]);
    expect(screen.getByText("Direct messages")).toBeInTheDocument();
  });

  it("sends exactly one source and channel per toggle", async () => {
    render(<NotificationsTab />);
    await userEvent.click(screen.getAllByRole("switch", { name: "Push" })[1]!);
    expect(state.mutateChannel).toHaveBeenCalledWith(
      { source: "dm", channel: "push", enabled: true },
      expect.any(Object),
    );
  });

  it("keeps the Push switches editable with no device registered (account-wide choice)", () => {
    render(<NotificationsTab />);
    for (const el of screen.getAllByRole("switch", { name: "Push" })) {
      expect(el).not.toHaveAttribute("aria-disabled", "true");
    }
  });

  it("shows the device section on web", () => {
    render(<NotificationsTab />);
    expect(screen.getByText("push device")).toBeInTheDocument();
  });

  it("never shows Push controls in Electron, but keeps the In-app switches", () => {
    state.desktop = true;
    render(<NotificationsTab />);
    expect(screen.queryByRole("switch", { name: "Push" })).not.toBeInTheDocument();
    expect(screen.queryByText("push device")).not.toBeInTheDocument();
    expect(screen.getAllByRole("switch", { name: "In-app" })).toHaveLength(2);
  });
});
