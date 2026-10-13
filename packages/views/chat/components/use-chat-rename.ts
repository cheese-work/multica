"use client";

import { useState } from "react";
import { toast } from "sonner";
import { useUpdateChatSession } from "@multica/core/chat/mutations";
import type { ChatSession } from "@multica/core/types";
import { useT } from "../../i18n";

export function useChatRename(session: ChatSession | null | undefined) {
  const { t } = useT("chat");
  const updateSession = useUpdateChatSession();
  const [renameRequest, setRenameRequest] = useState(0);

  const handleRename = async (raw: string): Promise<boolean> => {
    if (!session) {
      toast.error(t(($) => $.input.rename_session_required_toast));
      return false;
    }
    const title = raw.trim();
    if (!title) {
      setRenameRequest((request) => request + 1);
      return true;
    }
    if (title.length > 200) {
      toast.error(t(($) => $.input.rename_title_too_long_toast));
      return false;
    }
    if (title === session.title) return true;
    try {
      await updateSession.mutateAsync({ sessionId: session.id, title });
      return true;
    } catch {
      toast.error(t(($) => $.input.rename_failed_toast));
      return false;
    }
  };

  return { handleRename, renameRequest };
}
