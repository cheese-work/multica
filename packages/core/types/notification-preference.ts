export type NotificationGroupKey =
  | "unassigned_activity"
  | "assignments"
  | "status_changes"
  | "comments"
  | "mentions"
  | "updates"
  | "agent_activity"
  | "system_notifications";

export type NotificationGroupValue = "all" | "muted";

export type NotificationPreferences = Partial<Record<NotificationGroupKey, NotificationGroupValue>>;

export interface NotificationPreferenceResponse {
  workspace_id: string;
  preferences: NotificationPreferences;
}

export type NotificationChannelSource = "inbox" | "dm";

export type NotificationChannel = "in_app" | "push";

export type NotificationChannelSwitches = Record<NotificationChannel, boolean>;

export interface NotificationChannelsResponse {
  workspace_id: string;
  channels: Record<NotificationChannelSource, NotificationChannelSwitches>;
}

export interface UpdateNotificationChannelRequest {
  source: NotificationChannelSource;
  channel: NotificationChannel;
  enabled: boolean;
}
