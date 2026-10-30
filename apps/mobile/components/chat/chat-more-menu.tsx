/**
 * Reusable Chat more menu (CEO-6 / CHE-1760).
 *
 * Replaces the legacy delete-only alert on the ⋯ button in the chat header:
 *   - Statistics (`mobile-chat-more-statistics`)
 *   - Pin / Unpin (`mobile-chat-more-pin`, active DMs only)
 *   - Archive / Unarchive (`mobile-chat-more-archive`, disabled with pending
 *     label while the per-session mutation is in flight)
 *   - Delete (`mobile-chat-more-delete`, archived DMs only — mobile Delete
 *     stays Archived-only)
 */
import * as React from "react";
import { Ionicons } from "@expo/vector-icons";
import { useTheme } from "expo-router";
import type { ChatSession } from "@multica/core/types";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { IconButton } from "@/components/ui/icon-button";
import { Text } from "@/components/ui/text";
import { useChatSessionActionsStore } from "@/data/stores/chat-session-actions-store";
import { buildChatMoreMenuItems } from "@/lib/chat-dm-stats-archive";
import { useT } from "@/lib/i18n";

export interface ChatMoreMenuProps {
  session: ChatSession;
  onOpenStatistics: (session: ChatSession) => void;
  onTogglePin: (session: ChatSession) => void;
  onArchive: (session: ChatSession) => void;
  onUnarchive: (session: ChatSession) => void;
  onDelete: (session: ChatSession) => void;
}

export function ChatMoreMenu({
  session,
  onOpenStatistics,
  onTogglePin,
  onArchive,
  onUnarchive,
  onDelete,
}: ChatMoreMenuProps) {
  const { t } = useT("chat");
  const { colors } = useTheme();
  const actionPending = useChatSessionActionsStore(
    (s) => s.pendingBySession[session.id] ?? null,
  );
  const isArchived = session.status === "archived";

  const items = React.useMemo(
    () => buildChatMoreMenuItems({ session, actionPending }),
    [session, actionPending],
  );

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <IconButton
          name="ellipsis-horizontal"
          accessibilityLabel={t("header.more_actions")}
        />
      </DropdownMenuTrigger>
      <DropdownMenuContent
        testID="mobile-chat-more-menu"
        side="bottom"
        align="end"
        sideOffset={6}
        className="w-56"
      >
        {items.map((item) => {
          if (item.key === "statistics") {
            return (
              <DropdownMenuItem
                key={item.key}
                testID={item.testID}
                disabled={item.disabled}
                onPress={() => onOpenStatistics(session)}
                accessibilityLabel={t("header.statistics")}
                className="h-12 gap-3"
              >
                <Ionicons
                  name="stats-chart-outline"
                  size={18}
                  color={colors.text}
                />
                <Text>{t("header.statistics")}</Text>
              </DropdownMenuItem>
            );
          }

          if (item.key === "pin") {
            const label = session.pinned ? t("header.unpin") : t("header.pin");
            return (
              <DropdownMenuItem
                key={item.key}
                testID={item.testID}
                disabled={item.disabled}
                onPress={() => onTogglePin(session)}
                accessibilityLabel={label}
                className="h-12 gap-3"
              >
                <Ionicons
                  name={session.pinned ? "pin" : "pin-outline"}
                  size={18}
                  color={colors.text}
                />
                <Text>{label}</Text>
              </DropdownMenuItem>
            );
          }

          if (item.key === "archive") {
            const label = isArchived
              ? actionPending === "unarchive"
                ? t("header.unarchiving")
                : t("header.unarchive")
              : actionPending === "archive"
                ? t("header.archiving")
                : t("header.archive");
            return (
              <DropdownMenuItem
                key={item.key}
                testID={item.testID}
                disabled={item.disabled}
                onPress={() =>
                  isArchived ? onUnarchive(session) : onArchive(session)
                }
                accessibilityLabel={label}
                className="h-12 gap-3"
              >
                <Ionicons
                  name={isArchived ? "arrow-undo-outline" : "archive-outline"}
                  size={18}
                  color={colors.text}
                />
                <Text>{label}</Text>
              </DropdownMenuItem>
            );
          }

          return (
            <React.Fragment key={item.key}>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                testID={item.testID}
                variant="destructive"
                disabled={item.disabled}
                onPress={() => onDelete(session)}
                accessibilityLabel={t("header.delete")}
                className="h-12 gap-3"
              >
                <Ionicons
                  name="trash-outline"
                  size={18}
                  color={colors.notification}
                />
                <Text>{t("header.delete")}</Text>
              </DropdownMenuItem>
            </React.Fragment>
          );
        })}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
