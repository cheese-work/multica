export type NotificationGroupKey =
  | "unassigned_activity"
  | "assignments"
  | "status_changes"
  | "comments"
  | "chat_replies"
  | "mentions"
  | "reactions"
  | "updates"
  | "new_issues"
  | "agent_activity"
  | "system_notifications"
  | "auto_archive_completed";

export type NotificationGroupValue = "all" | "muted";

/**
 * How much of someone else's issue reaches you when you are only passively
 * subscribed (you filed it, commented, or were mentioned). Missing means
 * "key_updates": mentions, replies in your threads and handoffs.
 */
export type OthersIssuesValue = "all" | "key_updates";

export type NotificationPreferences = Partial<Record<NotificationGroupKey, NotificationGroupValue>> & {
  others_issues?: OthersIssuesValue;
};

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
