<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
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
vi.mock("./browser-notification-setting", () => ({
  BrowserNotificationSetting: () => null,
  browserNotificationSettingAvailable: () => false,
}));
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
||||||| 2ae2f08005
=======
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { notificationPreferenceKeys } from "@multica/core/notification-preferences/queries";
import enSettings from "../../locales/en/settings.json";
import { NotificationsTab } from "./notifications-tab";

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-a" }));
vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({ id: "ws-a", name: "Acme" }),
  useWorkspacePaths: () => ({ inbox: () => "/acme/inbox" }),
}));
vi.mock("../../navigation", () => ({
  AppLink: ({ href, children, className }: { href: string; children: ReactNode; className?: string }) => (
    <a href={href} className={className}>
      {children}
    </a>
  ),
}));
vi.mock("@multica/core/notification-preferences/mutations", () => ({
  useUpdateNotificationPreferences: () => ({ mutate: vi.fn() }),
}));
vi.mock("./browser-notification-setting", () => ({ BrowserNotificationSetting: () => null }));

afterEach(cleanup);

function renderTab(preferences: Record<string, string> = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity } } });
  qc.setQueryData(notificationPreferenceKeys.all("ws-a"), { workspace_id: "ws-a", preferences });
  render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={{ en: { settings: enSettings } }}>
        <NotificationsTab />
      </I18nProvider>
    </QueryClientProvider>,
  );
  return qc;
}

describe("Inbox notification settings", () => {
  it("groups the events by how they reach you", () => {
    renderTab({ updates: "muted" });
    const inbox = screen.getByRole("region", { name: "Inbox" });
    const lines = Array.from(
      inbox.querySelectorAll("[data-settings-anchor] .font-medium, .text-caption.font-medium"),
      (el) => el.textContent,
    );
    expect(lines).toEqual([
      "About you",
      "Assignments",
      "Mentions",
      "Reactions",
      "Issues you subscribe to",
      "Other people's issues",
      "Status changes",
      "Comments",
      "Priority & Due date",
      "Projects you subscribe to",
      "New issues",
      "Agents",
      "Agent activity",
    ]);
    expect(within(inbox).getByRole("switch", { name: "Priority & Due date" })).not.toBeChecked();
    expect(within(inbox).getByRole("switch", { name: "Comments" })).toBeChecked();
  });

  it.each<{ preferences: Record<string, string>; shown: string }>([
    { preferences: {}, shown: "Key updates only" },
    { preferences: { others_issues: "key_updates" }, shown: "Key updates only" },
    { preferences: { others_issues: "all" }, shown: "All activity" },
  ])("shows how much of other people's issues reaches you: %j", ({ preferences, shown }) => {
    renderTab(preferences);
    const inbox = screen.getByRole("region", { name: "Inbox" });
    expect(within(inbox).getByRole("combobox", { name: "Other people's issues" })).toHaveTextContent(shown);
  });

  it("explains how notifications leave the Inbox, with a way to the archived ones", () => {
    renderTab();
    const leaving = screen.getByRole("region", { name: "Leaving the Inbox" });
    expect(leaving).toHaveTextContent("Reading a notification doesn't take it out of the Inbox.");
    expect(leaving).toHaveTextContent("Issues others complete stay in your Inbox.");
    expect(within(leaving).getByRole("link", { name: "View archived" })).toHaveAttribute(
      "href",
      "/acme/inbox?view=archived",
    );
  });

  it.each<{ preferences: Record<string, string>; checked: boolean }>([
    { preferences: {}, checked: true },
    { preferences: { auto_archive_completed: "muted" }, checked: false },
  ])("toggles archiving issues you complete: %j", ({ preferences, checked }) => {
    renderTab(preferences);
    const leaving = screen.getByRole("region", { name: "Leaving the Inbox" });
    const toggle = within(leaving).getByRole("switch", { name: "Archive issues you complete" });
    if (checked) expect(toggle).toBeChecked();
    else expect(toggle).not.toBeChecked();
  });
});

describe("notification setting hierarchy", () => {
  it.each([
    { systemMuted: false, chatMuted: false },
    { systemMuted: false, chatMuted: true },
    { systemMuted: true, chatMuted: false },
    { systemMuted: true, chatMuted: true },
  ])("nests chat replies under system notifications: %j", ({ systemMuted, chatMuted }) => {
    const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity } } });
    qc.setQueryData(notificationPreferenceKeys.all("ws-a"), {
      workspace_id: "ws-a",
      preferences: {
        ...(systemMuted ? { system_notifications: "muted" } : {}),
        ...(chatMuted ? { chat_replies: "muted" } : {}),
      },
    });
    render(
      <QueryClientProvider client={qc}>
        <I18nProvider locale="en" resources={{ en: { settings: enSettings } }}>
          <NotificationsTab />
        </I18nProvider>
      </QueryClientProvider>,
    );
    expect(screen.queryByRole("region", { name: "Chat" })).toBeNull();
    const system = screen.getByRole("region", { name: "System Notifications" });
    if (systemMuted) {
      expect(within(system).queryByRole("switch", { name: "Include chat replies" })).toBeNull();
      expect(within(system).getAllByRole("switch")).toHaveLength(1);
    } else {
      const replies = within(system).getByRole("switch", { name: "Include chat replies" });
      if (chatMuted) expect(replies).not.toBeChecked();
      else expect(replies).toBeChecked();
      expect(within(system).getAllByRole("switch")).toHaveLength(2);
    }
    expect(within(system).queryByText("When an agent finishes replying in a chat.")).toBeNull();
    expect(within(system).queryByText("Enable system notifications first.")).toBeNull();
    expect(within(system).queryByText("Applies to Inbox notifications and chat replies.")).toBeNull();
    qc.clear();
  });
});
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
