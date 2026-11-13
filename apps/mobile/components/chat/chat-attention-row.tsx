/**
 * DM attention line under a chat-list row: kind, level and age as text with an
 * icon (never colour alone, E28), plus the Xong (Done) and Để sau (Later)
 * actions. Renders nothing when the session owes Cheese nothing.
 */
import { useEffect, useRef, useState } from "react";
import { Alert, Pressable, View } from "react-native";
import { Ionicons } from "@expo/vector-icons";
import { useTheme } from "expo-router";
import type { ChatSession } from "@multica/core/types";
import { Text } from "@/components/ui/text";
import {
  useChatAttentionLater,
  useChatAttentionXong,
} from "@/data/mutations/chat-attention";
import { attentionAge, attentionRowState } from "@/lib/chat-attention";
import { useT } from "@/lib/i18n";
import { createIdempotencyKey } from "@/lib/request-id";
import { cn } from "@/lib/utils";

const LEVEL_ICON = {
  fresh: "ellipse-outline",
  yellow: "time-outline",
  red: "alert-circle",
} as const;

// Same hues as the text-warning / text-destructive classes below.
const LEVEL_COLOR = { yellow: "#d9a30a", red: "#ef4444" } as const;

const LEVEL_TEXT = {
  fresh: "text-muted-foreground",
  yellow: "text-warning",
  red: "text-destructive",
} as const;

export function ChatAttentionRow({ session }: { session: ChatSession }) {
  const { t } = useT("chat");
  const xong = useChatAttentionXong();
  const later = useChatAttentionLater();
  const { colors } = useTheme();
  // Age text re-reads the clock each minute while the row is on screen.
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 60_000);
    return () => clearInterval(timer);
  }, []);
  // One request id per snapshot: a retry after a failure reuses it (idempotent),
  // a success or a changed snapshot mints a new one.
  const laterRequest = useRef<{ keys: string; id: string } | null>(null);
  const row = attentionRowState(session);
  if (!row) return null;
  const requestIdFor = (keys: string[]) => {
    const sig = keys.join("|");
    if (laterRequest.current?.keys !== sig) {
      laterRequest.current = { keys: sig, id: createIdempotencyKey() };
    }
    return laterRequest.current.id;
  };

  const title = session.title || t("sessions.new_chat");
  const kind = t(`attention.kind.${row.source}`);
  const level = t(`attention.level.${row.level}`);
  const age = row.since ? attentionAge(row.since, now) : null;
  const ageText = age ? t(`attention.age.${age.unit}`, { count: age.count }) : "";
  const onError = () => Alert.alert(t("attention.failed"));

  return (
    <View
      testID="mobile-chat-attention-row"
      className="flex-row items-center gap-2 pl-[68px] pr-4 pb-2"
    >
      <View
        className="flex-1 flex-row items-center gap-1"
        accessible
        accessibilityLabel={t("attention.row_a11y", { kind, level, age: ageText })}
      >
        <Ionicons
          name={LEVEL_ICON[row.level]}
          size={14}
          color={row.level === "fresh" ? colors.text : LEVEL_COLOR[row.level]}
        />
        <Text className={cn("text-xs font-medium", LEVEL_TEXT[row.level])} numberOfLines={1}>
          {kind} · {ageText ? t("attention.level_age", { level, age: ageText }) : level}
        </Text>
      </View>
      {row.canXong ? (
        <Pressable
          testID="mobile-chat-attention-xong"
          accessibilityRole="button"
          accessibilityLabel={t("attention.done_a11y", { title })}
          disabled={xong.isPending}
          onPress={() => xong.mutate({ sessionId: session.id, keys: row.keys }, { onError })}
          className="min-h-[44px] justify-center px-3 active:opacity-60"
        >
          <Text className="text-xs font-semibold text-primary">{t("attention.done")}</Text>
        </Pressable>
      ) : null}
      {row.canLater ? (
        <Pressable
          testID="mobile-chat-attention-later"
          accessibilityRole="button"
          accessibilityLabel={t("attention.later_a11y", { title })}
          disabled={later.isPending}
          onPress={() =>
            later.mutate(
              { sessionId: session.id, keys: row.keys, requestId: requestIdFor(row.keys) },
              {
                onSuccess: () => {
                  laterRequest.current = null;
                },
                onError,
              },
            )
          }
          className="min-h-[44px] justify-center px-3 active:opacity-60"
        >
          <Text className="text-xs font-semibold text-primary">{t("attention.later")}</Text>
        </Pressable>
      ) : null}
    </View>
  );
}
