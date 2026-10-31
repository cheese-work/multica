"use client";

import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  notificationChannelOptions,
  useUpdateNotificationChannel,
} from "@multica/core/notification-preferences/channels";
import { notificationPreferenceOptions } from "@multica/core/notification-preferences/queries";
import { useUpdateNotificationPreferences } from "@multica/core/notification-preferences/mutations";
import type {
  NotificationChannel,
  NotificationChannelSource,
  NotificationGroupKey,
  NotificationPreferences,
} from "@multica/core/types";
import { Switch } from "@multica/ui/components/ui/switch";
import { toast } from "sonner";
import { useT } from "../../i18n";
import { isDesktopShell } from "../../platform";
import { BrowserNotificationSetting } from "./browser-notification-setting";
import { PushDeviceSetting } from "./push-device-setting";
import {
  SettingsCard,
  SettingsRow,
  SettingsSection,
  SettingsTab,
} from "./settings-layout";

// Inbox event groups rendered in the per-event toggle list. `system_notifications`
// is a sibling preference key but lives in its own section below.
const INBOX_GROUP_KEYS = [
  "assignments",
  "status_changes",
  "comments",
  "mentions",
  "updates",
  "agent_activity",
] as const;
type InboxGroupKey = (typeof INBOX_GROUP_KEYS)[number];

const CHANNELS: readonly NotificationChannel[] = ["in_app", "push"];

export function NotificationsTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const { data } = useQuery(notificationPreferenceOptions(wsId));
  const { data: channelData } = useQuery(notificationChannelOptions(wsId));
  const mutation = useUpdateNotificationPreferences();
  const channelMutation = useUpdateNotificationChannel();

  // Push is web-only. The desktop shell is only known after mount, so the first
  // paint matches the server markup.
  const [mounted, setMounted] = useState(false);
  useEffect(() => setMounted(true), []);
  const showPush = mounted && !isDesktopShell();

  const handleChannel = (
    source: NotificationChannelSource,
    channel: NotificationChannel,
    enabled: boolean,
  ) => {
    channelMutation.mutate(
      { source, channel, enabled },
      {
        onError: (err) =>
          toast.error(
            err instanceof Error && err.message
              ? err.message
              : t(($) => $.notifications.toast_failed),
          ),
      },
    );
  };

  const channelRows = (source: NotificationChannelSource) =>
    CHANNELS.filter((channel) => channel === "in_app" || showPush).map((channel) => (
      <SettingsRow
        key={channel}
        anchor={`${source}_${channel}`}
        label={t(($) => $.notifications.channels[channel])}
      >
        <Switch
          checked={channelData?.channels[source][channel] ?? false}
          aria-label={t(($) => $.notifications.channels[channel])}
          onCheckedChange={(checked) => handleChannel(source, channel, checked)}
        />
      </SettingsRow>
    ));

  const preferences = data?.preferences ?? {};

  const handleToggle = (key: NotificationGroupKey, enabled: boolean) => {
    const updated: NotificationPreferences = {
      ...preferences,
      [key]: enabled ? "all" : "muted",
    };
    // Remove keys set to "all" (default) to keep the object clean
    if (enabled) {
      delete updated[key];
    }
    mutation.mutate(updated, {
      onError: (err) =>
        toast.error(
          err instanceof Error && err.message
            ? err.message
            : t(($) => $.notifications.toast_failed),
        ),
    });
  };

  const systemEnabled = preferences.system_notifications !== "muted";
  const focusedDelivery = preferences.unassigned_activity === "muted";

  return (
    <SettingsTab title={t(($) => $.page.tabs.notifications)} scope="workspace-only">
      <SettingsSection
        title={t(($) => $.notifications.title)}
        description={t(($) => $.notifications.description)}
        anchor="inbox"
      >
        <SettingsCard>
            {channelRows("inbox")}
            <SettingsRow
              anchor="unassigned_activity"
              label={t(($) => $.notifications.focused_delivery)}
            >
              <Switch
                checked={focusedDelivery}
                aria-label={t(($) => $.notifications.focused_delivery)}
                onCheckedChange={(checked) => handleToggle("unassigned_activity", !checked)}
              />
            </SettingsRow>
            {INBOX_GROUP_KEYS.map((key: InboxGroupKey) => {
              const enabled = preferences[key] !== "muted";
              return (
                <SettingsRow
                  key={key}
                  anchor={key}
                  label={t(($) => $.notifications.groups[key].label)}
                  description={t(($) => $.notifications.groups[key].description)}
                >
                  <Switch
                    checked={enabled}
                    aria-label={t(($) => $.notifications.groups[key].label)}
                    onCheckedChange={(checked) => handleToggle(key, checked)}
                  />
                </SettingsRow>
              );
            })}
        </SettingsCard>
      </SettingsSection>

      <SettingsSection title={t(($) => $.notifications.dm.title)} anchor="dm">
        <SettingsCard>{channelRows("dm")}</SettingsCard>
      </SettingsSection>

      <SettingsSection
        title={t(($) => $.notifications.system.title)}
      >
        <SettingsCard>
          <SettingsRow
            anchor="system"
            label={t(($) => $.notifications.system.label)}
          >
              <Switch
                checked={systemEnabled}
                aria-label={t(($) => $.notifications.system.label)}
                onCheckedChange={(checked) => handleToggle("system_notifications", checked)}
              />
          </SettingsRow>
        </SettingsCard>

        {/* Web-only: the browser permission banners require. Renders nothing on
            desktop (OS-native delivery) or where the Notification API is absent. */}
        <BrowserNotificationSetting />
      </SettingsSection>

      {showPush && (
        <SettingsSection title={t(($) => $.notifications.device.title)} anchor="device">
          <PushDeviceSetting />
        </SettingsSection>
      )}
    </SettingsTab>
  );
}
