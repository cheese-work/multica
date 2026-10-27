/**
 * Chat session-switch sheet — presented as a formSheet by the parent Stack.
 *
 * Splits the workspace's `status=all` sessions cache into:
 *   - Active sessions (`mobile-chat-list-pinned`): pinned rows first, then by
 *     recent activity. Active rows never expose Delete ("Mobile Delete stays
 *     Archived-only").
 *   - Archived sessions (`mobile-chat-list-archived`): collapsible section
 *     sorted strictly by activity descending, ignoring `pinned` so archived
 *     rows never surface a pinned category or pin icon. Each archived row
 *     exposes a 44pt delete button (`mobile-chat-session-delete`).
 */
import { useMemo, useState } from "react";
import { Alert, Pressable, ScrollView, View } from "react-native";
import { Ionicons } from "@expo/vector-icons";
import { router, useTheme } from "expo-router";
import { useQuery } from "@tanstack/react-query";
import type { ChatSession } from "@multica/core/types";
import { ActorAvatar } from "@/components/ui/actor-avatar";
import { IconButton } from "@/components/ui/icon-button";
import { Text } from "@/components/ui/text";
import { useDeleteChatSession } from "@/data/mutations/chat";
import { chatSessionsOptions } from "@/data/queries/chat";
import { useChatSessionPickerStore } from "@/data/stores/chat-session-picker-store";
import { useWorkspaceStore } from "@/data/workspace-store";
import { splitChatSessions } from "@/lib/chat-dm-stats-archive";
import { useT } from "@/lib/i18n";
import { cn } from "@/lib/utils";

export default function ChatSessionsRoute() {
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  const { t } = useT("chat");
  const { colors } = useTheme();
  const { data: sessions = [] } = useQuery(chatSessionsOptions(wsId));
  const activeSessionId = useChatSessionPickerStore((s) => s.activeSessionId);
  const requestSelect = useChatSessionPickerStore((s) => s.requestSelect);
  const deleteSession = useDeleteChatSession();

  const { active, archived } = useMemo(
    () => splitChatSessions(sessions),
    [sessions],
  );

  const activeIsArchived = useMemo(
    () => archived.some((s) => s.id === activeSessionId),
    [archived, activeSessionId],
  );

  const [archivedOpen, setArchivedOpen] = useState(
    () => activeIsArchived || active.length === 0,
  );

  // Open the archive group when the active session moves into it. Adjusted
  // during render (no effect) so it cannot cascade an extra render.
  const [prevActiveIsArchived, setPrevActiveIsArchived] = useState(activeIsArchived);
  if (activeIsArchived !== prevActiveIsArchived) {
    setPrevActiveIsArchived(activeIsArchived);
    if (activeIsArchived) setArchivedOpen(true);
  }

  const confirmDeleteArchived = (session: ChatSession) => {
    if (session.status !== "archived") return;
    Alert.alert(
      t("alerts.delete_title"),
      session.title || t("sessions.new_chat"),
      [
        { text: t("common:actions.cancel"), style: "cancel" },
        {
          text: t("alerts.delete_confirm"),
          style: "destructive",
          onPress: () => {
            deleteSession.mutate(session.id);
            if (session.id === activeSessionId) {
              requestSelect(null);
            }
          },
        },
      ],
      { cancelable: true },
    );
  };

  return (
    <View testID="mobile-chat-list" className="flex-1 bg-background">
      <View className="px-4 pt-4 pb-3">
        <Text className="text-base font-semibold text-foreground">
          {t("sessions.title")}
        </Text>
      </View>
      <ScrollView className="flex-1" showsVerticalScrollIndicator={false}>
        {active.length === 0 && archived.length === 0 ? (
          <View className="px-4 py-8">
            <Text className="text-center text-sm text-muted-foreground">
              {t("sessions.empty")}
            </Text>
          </View>
        ) : (
          <>
            <View testID="mobile-chat-list-pinned">
              {active.map((session) => {
                const selected = session.id === activeSessionId;
                return (
                  <Pressable
                    key={session.id}
                    testID="mobile-chat-session-row"
                    onPress={() => {
                      requestSelect(session.id);
                      router.back();
                    }}
                    className={cn(
                      "min-h-[52px] flex-row items-center gap-3 px-4 py-3 active:bg-secondary",
                      selected && "bg-secondary/60",
                    )}
                  >
                    <View
                      className={cn(
                        "h-2 w-2 rounded-full",
                        session.has_unread ? "bg-primary" : "bg-transparent",
                      )}
                    />
                    <ActorAvatar
                      type="agent"
                      id={session.agent_id}
                      size={32}
                      showPresence
                    />
                    <View className="flex-1">
                      <Text
                        className={cn(
                          "text-sm text-foreground",
                          session.has_unread && "font-semibold",
                        )}
                        numberOfLines={1}
                      >
                        {session.title || t("sessions.new_chat")}
                      </Text>
                    </View>
                    {session.pinned ? (
                      <Ionicons
                        name="pin"
                        size={14}
                        color={colors.text}
                        accessibilityLabel={t("sessions.pinned")}
                      />
                    ) : null}
                    {selected ? (
                      <Text className="text-sm font-semibold text-primary">
                        ✓
                      </Text>
                    ) : null}
                  </Pressable>
                );
              })}
            </View>

            {archived.length > 0 ? (
              <View
                testID="mobile-chat-list-archived"
                className="mt-2 border-t border-border pt-2"
              >
                <Pressable
                  onPress={() => setArchivedOpen((prev) => !prev)}
                  accessibilityRole="button"
                  accessibilityState={{ expanded: archivedOpen }}
                  className="min-h-[44px] flex-row items-center justify-between px-4 py-2 active:bg-secondary"
                >
                  <Text className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                    {t("sessions.archived_title")} · {archived.length}
                  </Text>
                  <Ionicons
                    name={archivedOpen ? "chevron-down" : "chevron-forward"}
                    size={16}
                    color={colors.text}
                  />
                </Pressable>

                {archivedOpen
                  ? archived.map((session) => {
                      const selected = session.id === activeSessionId;
                      return (
                        <View
                          key={session.id}
                          testID="mobile-chat-session-row"
                          className={cn(
                            "min-h-[52px] flex-row items-center gap-2 pl-4 pr-2 py-2",
                            selected && "bg-secondary/60",
                          )}
                        >
                          <Pressable
                            onPress={() => {
                              requestSelect(session.id);
                              router.back();
                            }}
                            onLongPress={() => confirmDeleteArchived(session)}
                            className="flex-1 flex-row items-center gap-3 py-1 active:opacity-70"
                          >
                            <View className="h-2 w-2 rounded-full bg-transparent" />
                            <ActorAvatar
                              type="agent"
                              id={session.agent_id}
                              size={32}
                              showPresence
                            />
                            <View className="flex-1">
                              <Text
                                className="text-sm text-foreground"
                                numberOfLines={1}
                              >
                                {session.title || t("sessions.new_chat")}
                              </Text>
                              <Text className="mt-0.5 text-xs text-muted-foreground">
                                {t("sessions.archived")}
                              </Text>
                            </View>
                            {selected ? (
                              <Text className="text-sm font-semibold text-primary">
                                ✓
                              </Text>
                            ) : null}
                          </Pressable>
                          <IconButton
                            testID="mobile-chat-session-delete"
                            name="trash-outline"
                            iconSize={18}
                            color={
                              typeof colors.notification === "string"
                                ? colors.notification
                                : undefined
                            }
                            onPress={() => confirmDeleteArchived(session)}
                            accessibilityLabel={t("header.delete")}
                            className="h-11 w-11"
                          />
                        </View>
                      );
                    })
                  : null}
              </View>
            ) : null}
          </>
        )}
      </ScrollView>
    </View>
  );
}
