import { useEffect, useMemo, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import type { ChatSession } from "@multica/core/types";
import { chatKeys } from "@/data/queries/chat";
import { attentionRefreshDelay, nextAttentionChange } from "@/lib/chat-attention";

/** Refetches per instant when the server keeps returning the same one. */
const MAX_RETRIES = 5;

/**
 * Refetches the sessions list when the server says the DM attention projection
 * changes (a later reminder falls due, a 4 h or 24 h crossing), so a hidden
 * row appears without navigation or reload (A25). Focus and reconnect are
 * covered by the QueryClient lifecycle.
 */
export function useAttentionRefresh(wsId: string | null, sessions: ChatSession[]) {
  const qc = useQueryClient();
  const instant = useMemo(() => nextAttentionChange(sessions), [sessions]);
  // A refetch that returns the same instant would not re-run the effect;
  // `attempt` re-arms it, and a new instant starts the count over.
  const [attempt, setAttempt] = useState({ instant, n: 0 });
  const n = attempt.instant === instant ? attempt.n : 0;
  useEffect(() => {
    const delay = attentionRefreshDelay(sessions, Date.now());
    if (delay === null || n >= MAX_RETRIES) return;
    const timer = setTimeout(() => {
      void qc.invalidateQueries({ queryKey: chatKeys.sessions(wsId) });
      setAttempt({ instant, n: n + 1 });
    }, delay);
    return () => clearTimeout(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `instant` stands for `sessions`
  }, [qc, wsId, instant, n]);
}
