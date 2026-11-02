/**
 * In-place compaction marker (CHE-2169): a divider where the chat was
 * compacted; the summary expands underneath. Web: CompactionDivider.
 */
import { Pressable, View } from "react-native";
import type { ChatMessage } from "@multica/core/types";
import { Text } from "@/components/ui/text";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";
import { Markdown } from "@/lib/markdown";
import { formatCompactionTime } from "@/lib/compaction-time";
import { useT } from "@/lib/i18n";

export function CompactionDivider({
  message,
  isSelecting,
}: {
  message: ChatMessage;
  isSelecting: boolean;
}) {
  const { t } = useT("chat");
  const time = formatCompactionTime(message.created_at);
  const label = time
    ? t("compaction_divider", { time })
    : t("compaction_divider_untimed", {});
  return (
    <Collapsible>
      <CollapsibleTrigger asChild>
        <Pressable
          accessibilityRole="button"
          accessibilityLabel={label}
          className="flex-row items-center gap-3 active:opacity-70"
        >
          <View className="h-px flex-1 bg-border" />
          <Text className="text-xs text-muted-foreground">{label}</Text>
          <View className="h-px flex-1 bg-border" />
        </Pressable>
      </CollapsibleTrigger>
      <CollapsibleContent>
        <View className="mt-2">
          <Markdown
            content={message.content}
            attachments={message.attachments}
            selectable={isSelecting}
          />
        </View>
      </CollapsibleContent>
    </Collapsible>
  );
}
