/**
 * Queued DM follow-ups above the composer, each with a Steer action that adds
 * the message to the running reply (CHE-2820). Mirror of the Steer part of
 * `packages/views/chat/components/chat-queue.tsx`; the state rules live in
 * `lib/chat-steer.ts`.
 *
 * Divergence: mobile has no `running_agent_reply` setting, so a follow-up
 * always queues first and Steer is an explicit tap. When Steer is off, the
 * reason shows as visible text and the message stays queued. Stop & send,
 * edit and remove are not on mobile. The list is height-capped and scrolls,
 * so a long queue cannot push the composer off screen.
 */
import { useState } from "react";
import { Alert, Pressable, ScrollView, View } from "react-native";
import { useQueryClient } from "@tanstack/react-query";
import type { ChatHeadSupplement, ChatQueuedTask } from "@multica/core/types";
import { api } from "@/data/api";
import { chatKeys } from "@/data/queries/chat";
import { FOLLOW_UP_ROW_HEIGHT, followUpListMaxHeight, presentSteer, steerQueuedTask } from "@/lib/chat-steer";
import { Text } from "@/components/ui/text";
import { useT } from "@/lib/i18n";

interface Props {
  sessionId: string;
  tasks: ChatQueuedTask[];
  headStatus: string | undefined;
  headSupplement: ChatHeadSupplement | undefined;
  /** The caller may no longer run this agent. */
  noPermission?: boolean;
}

export function QueuedFollowUps({ sessionId, tasks, headStatus, headSupplement, noPermission }: Props) {
  const { t } = useT("chat");
  const qc = useQueryClient();
  const [busyTaskId, setBusyTaskId] = useState<string | null>(null);
  if (tasks.length === 0) return null;

  const { canSteer, reasonKey } = presentSteer({ headStatus, headSupplement, noPermission });

  const steer = async (taskId: string) => {
    setBusyTaskId(taskId);
    const outcome = await steerQueuedTask(sessionId, taskId, {
      steer: (s, id) => api.steerChatTask(s, id),
      refresh: (s) => {
        qc.invalidateQueries({ queryKey: chatKeys.messages(s) });
        qc.invalidateQueries({ queryKey: chatKeys.pendingTask(s) });
      },
    });
    setBusyTaskId(null);
    if (outcome === "failed") Alert.alert(t("steer.failed"));
  };

  return (
    <View testID="mobile-chat-queued-follow-ups" className="mx-3 mb-1 rounded-lg border border-border bg-card px-3 py-2">
      {reasonKey ? <Text className="mb-1 text-xs text-muted-foreground">{t(reasonKey)}</Text> : null}
      <ScrollView
        style={{ maxHeight: followUpListMaxHeight(tasks.length) }}
        keyboardShouldPersistTaps="handled"
        nestedScrollEnabled
      >
      {tasks.map((task) => (
        <View key={task.task_id} style={{ height: FOLLOW_UP_ROW_HEIGHT }} className="flex-row items-center gap-2">
          <Text numberOfLines={1} className="flex-1 text-sm text-muted-foreground">
            {task.content?.trim() || t("steer.fallback")}
          </Text>
          <Pressable
            accessibilityRole="button"
            accessibilityState={{ disabled: !canSteer || busyTaskId !== null }}
            disabled={!canSteer || busyTaskId !== null}
            onPress={() => void steer(task.task_id)}
            hitSlop={8}
            className="active:opacity-70"
          >
            <Text className={canSteer ? "text-sm font-medium text-foreground" : "text-sm text-muted-foreground"}>
              {t("steer.label")}
            </Text>
          </Pressable>
        </View>
      ))}
      </ScrollView>
    </View>
  );
}
