/**
 * Right-side actions for the Chat tab header. Two controls:
 *   - ⋯ (`ChatMoreMenu`): shown when an active session exists.
 *   - + (new chat): always shown.
 */
import type { ChatSession } from "@multica/core/types";
import {
  ChatMoreMenu,
  type ChatMoreMenuProps,
} from "@/components/chat/chat-more-menu";
import { IconButton } from "@/components/ui/icon-button";
import { useT } from "@/lib/i18n";

interface Props extends Omit<ChatMoreMenuProps, "session"> {
  session: ChatSession | null;
  onNewPress: () => void;
}

export function ChatSessionActions({
  session,
  onOpenStatistics,
  onTogglePin,
  onArchive,
  onUnarchive,
  onDelete,
  onNewPress,
}: Props) {
  const { t } = useT("chat");
  return (
    <>
      {session ? (
        <ChatMoreMenu
          session={session}
          onOpenStatistics={onOpenStatistics}
          onTogglePin={onTogglePin}
          onArchive={onArchive}
          onUnarchive={onUnarchive}
          onDelete={onDelete}
        />
      ) : null}
      <IconButton
        name="add"
        iconSize={24}
        onPress={onNewPress}
        accessibilityLabel={t("a11y.new_chat")}
      />
    </>
  );
}
