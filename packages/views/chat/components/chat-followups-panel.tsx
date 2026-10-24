"use client";

import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { chatWakeupsOptions } from "@multica/core/chat/queries";
import { useCancelChatWakeup } from "@multica/core/chat/mutations";
import { useOnline } from "../../settings/components/use-online";
import { ChatFollowUps } from "./chat-followups";

interface ChatFollowUpsPanelProps {
  /** The open DM; null for a draft with no session yet. */
  sessionId: string | null;
  agentName?: string;
}

// A first load is usually instant and usually empty; show the loading row only
// when it is slow, so opening a DM with no follow-up never flashes a strip.
const LOADING_VISIBLE_AFTER_MS = 300;

function useSlowLoading(loading: boolean): boolean {
  const [slow, setSlow] = useState(false);
  useEffect(() => {
    if (!loading) {
      setSlow(false);
      return;
    }
    const timer = setTimeout(() => setSlow(true), LOADING_VISIBLE_AFTER_MS);
    return () => clearTimeout(timer);
  }, [loading]);
  return slow;
}

// Wires the follow-up strip to the member list/cancel endpoints. The list is
// the server's, so a cancel only removes a row once the refetch says so.
export function ChatFollowUpsPanel({ sessionId, agentName }: ChatFollowUpsPanelProps) {
  const id = sessionId ?? "";
  const online = useOnline();
  const list = useQuery(chatWakeupsOptions(id));
  const cancel = useCancelChatWakeup(id);
  const slowLoading = useSlowLoading(!!sessionId && !list.data && !list.isError);

  if (!sessionId) return null;

  const status = list.isError && !list.data ? "error" : list.data ? "ready" : "loading";
  if (status === "loading" && !slowLoading) return null;
  return (
    <ChatFollowUps
      sessionId={sessionId}
      agentName={agentName}
      wakeups={list.data?.wakeups ?? []}
      status={status}
      cancellingId={cancel.isPending ? cancel.variables : null}
      cancelErrorId={cancel.isError ? cancel.variables : null}
      offline={!online}
      onCancel={(wakeupId) => cancel.mutate(wakeupId)}
      onRetry={() => void list.refetch()}
    />
  );
}
