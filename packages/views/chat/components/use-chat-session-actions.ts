"use client";

import { useCallback } from "react";
import { toast } from "sonner";
import { useChatStore } from "@multica/core/chat";
import {
  useSetChatSessionArchived,
  useSetChatSessionPinned,
} from "@multica/core/chat/mutations";
import type { ChatSession } from "@multica/core/types";
import { useT } from "../../i18n";

const UNDO_WINDOW_MS = 5000;

interface ChatSessionActions {
  /**
   * Archives after the server accepts it, then runs `onArchived` (the caller
   * moves the pane off the session). On failure the session stays open and the
   * server's reason is shown. Resolves false when nothing was archived.
   */
  archive: (session: ChatSession, options?: { onArchived?: () => void }) => Promise<boolean>;
  unarchive: (session: ChatSession) => Promise<boolean>;
  /** Pins or unpins an active DM; an archived DM is never pinned. */
  togglePin: (session: ChatSession) => Promise<boolean>;
}

/**
 * The one door for archive, unarchive and pin from every surface (thread list
 * rows, the header menu, the floating window and the slash commands). Archive
 * and unarchive share a per-session guard in the chat store, so the menu and a
 * command cannot both run for the same DM.
 */
export function useChatSessionActions(): ChatSessionActions {
  const { t } = useT("chat");
  const setArchived = useSetChatSessionArchived();
  const setPinned = useSetChatSessionPinned();

  const errorText = useCallback(
    (err: unknown, fallback: string) =>
      err instanceof Error && err.message ? err.message : fallback,
    [],
  );

  const unarchive = useCallback(
    async (session: ChatSession): Promise<boolean> => {
      const store = useChatStore.getState();
      if (!store.beginSessionAction(session.id, "unarchive")) return false;
      try {
        await setArchived.mutateAsync({ sessionId: session.id, archived: false });
        return true;
      } catch (err) {
        toast.error(errorText(err, t(($) => $.list.unarchive_failed)));
        return false;
      } finally {
        useChatStore.getState().endSessionAction(session.id);
      }
    },
    [errorText, setArchived, t],
  );

  const archive = useCallback(
    async (session: ChatSession, options?: { onArchived?: () => void }): Promise<boolean> => {
      const store = useChatStore.getState();
      if (!store.beginSessionAction(session.id, "archive")) return false;
      try {
        await setArchived.mutateAsync({ sessionId: session.id, archived: true });
      } catch (err) {
        toast.error(errorText(err, t(($) => $.list.archive_failed)));
        return false;
      } finally {
        useChatStore.getState().endSessionAction(session.id);
      }
      options?.onArchived?.();
      toast(t(($) => $.list.archived_toast), {
        duration: UNDO_WINDOW_MS,
        action: {
          label: t(($) => $.list.undo),
          onClick: () => void unarchive(session),
        },
      });
      return true;
    },
    [errorText, setArchived, t, unarchive],
  );

  const togglePin = useCallback(
    async (session: ChatSession): Promise<boolean> => {
      if (session.status === "archived") return false;
      try {
        await setPinned.mutateAsync({ sessionId: session.id, pinned: !session.pinned });
        return true;
      } catch (err) {
        toast.error(errorText(err, t(($) => $.list.pin_failed)));
        return false;
      }
    },
    [errorText, setPinned, t],
  );

  return { archive, unarchive, togglePin };
}
