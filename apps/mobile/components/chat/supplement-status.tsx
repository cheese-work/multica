/**
 * Delivery state under a user bubble whose follow-up was steered into the
 * running reply (CHE-2820). Mirror of
 * `packages/views/chat/components/chat-supplement-status.tsx`. The state
 * mapping lives in `lib/chat-supplement.ts`.
 *
 * Steer itself starts in `queued-follow-ups.tsx`. This row shows the receipt
 * and offers the resend actions. An `uncertain` resend asks for confirmation
 * through a native alert first.
 */
import { useState } from "react";
import { Alert, Pressable, View } from "react-native";
import { useQueryClient } from "@tanstack/react-query";
import type { ChatSupplement } from "@multica/core/types";
import { api } from "@/data/api";
import { chatKeys } from "@/data/queries/chat";
import { presentSupplement } from "@/lib/chat-supplement";
import { Text } from "@/components/ui/text";
import { useT } from "@/lib/i18n";

export function SupplementStatus({ supplement }: { supplement: ChatSupplement }) {
  const { t } = useT("chat");
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const { labelKey, action, resent } = presentSupplement(supplement);

  const resend = async (confirmPossibleDuplicate: boolean) => {
    setBusy(true);
    try {
      await api.resendChatSupplement(supplement.chat_session_id, supplement.message_id, {
        confirmPossibleDuplicate,
      });
    } catch {
      Alert.alert(t("supplement.resend_failed"));
    } finally {
      setBusy(false);
      qc.invalidateQueries({ queryKey: chatKeys.messages(supplement.chat_session_id) });
      qc.invalidateQueries({ queryKey: chatKeys.pendingTask(supplement.chat_session_id) });
    }
  };

  const onPress = () => {
    if (action === "resend") {
      void resend(false);
      return;
    }
    Alert.alert(t("supplement.confirm_title"), t("supplement.confirm_body"), [
      { text: t("supplement.cancel"), style: "cancel" },
      { text: t("supplement.confirm_send"), onPress: () => void resend(true) },
    ]);
  };

  return (
    <View className="mt-1 flex-row flex-wrap items-center justify-end gap-x-2 self-end">
      <Text className="text-xs text-muted-foreground">{t(labelKey)}</Text>
      {resent ? <Text className="text-xs text-muted-foreground">{t("supplement.resent")}</Text> : null}
      {action !== "none" ? (
        <Pressable
          accessibilityRole="button"
          disabled={busy}
          onPress={onPress}
          hitSlop={8}
          className="active:opacity-70"
        >
          <Text className="text-xs font-medium text-foreground">
            {t(action === "resend" ? "supplement.send_new_run" : "supplement.send_again_anyway")}
          </Text>
        </Pressable>
      ) : null}
    </View>
  );
}
