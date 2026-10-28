import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, renderHook, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { NotificationPreferences } from "@multica/core/types";
import enSettings from "../../locales/en/settings.json";

const state = vi.hoisted(() => ({
  preferences: {} as NotificationPreferences,
  mutate: vi.fn(),
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace" }));
vi.mock("@tanstack/react-query", () => ({
  queryOptions: (options: unknown) => options,
  useQuery: () => ({ data: { preferences: state.preferences } }),
}));
vi.mock("@multica/core/notification-preferences/mutations", () => ({
  useUpdateNotificationPreferences: () => ({ mutate: state.mutate }),
}));
vi.mock("../../i18n", () => ({
  useT: () => ({ t: (select: (messages: typeof enSettings) => string) => select(enSettings) }),
}));
vi.mock("./browser-notification-setting", () => ({ BrowserNotificationSetting: () => null }));
vi.mock("./settings-layout", () => ({
  SettingsTab: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  SettingsSection: ({ children }: { children: ReactNode }) => <section>{children}</section>,
  SettingsCard: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  SettingsRow: ({ children, label }: { children: ReactNode; label: string }) => <div>{label}{children}</div>,
}));

import { NotificationsTab } from "./notifications-tab";
import { useSettingsSearchIndex } from "./use-settings-search-index";

describe("notification delivery scope", () => {
  beforeEach(() => {
    cleanup();
    state.preferences = {};
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
