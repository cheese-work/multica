"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { chatKeys } from "@multica/core/chat/queries";
import type { ChatSession } from "@multica/core/types";

/** Wait past the server's instant so the refetch lands after the change. */
const REFRESH_SLACK_MS = 500;
/** Floor for a refetch timer, so a stale next_change_at cannot spin. */
const REFRESH_MIN_MS = 5_000;
/** setTimeout overflows past 2^31-1 ms; the refetch is harmless if early. */
const REFRESH_MAX_MS = 2_147_483_647;
/** Refetches per next_change_at value when the server keeps returning it. */
const REFRESH_MAX_RETRIES = 5;

/**
 * Keeps the DM attention projection fresh (needs-you levels and counts).
 * `enabled` lets a caller that already runs it turn a nested copy off.
 */
export function useAttentionRefresh(sessions: ChatSession[], enabled = true) {
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  // The list query never goes stale on its own (staleTime: Infinity), so the
  // projection is refreshed when the server says it changes, and whenever the
  // window can have missed time passing. WS reconnects are handled in realtime.
  const nextChange = useMemo(
    () => Math.min(...sessions.map((s) => Date.parse(s.next_change_at ?? "")).filter(Number.isFinite)),
    [sessions],
  );
  // A refetch that returns the same next_change_at (server clock behind, failed
  // fetch, early timeout) would not re-run the effect; `attempt` re-arms it.
  const [attempt, setAttempt] = useState(0);
  const retries = useRef({ for: nextChange, n: 0 });
  useEffect(() => {
    if (!enabled) return;
    const refresh = () => void qc.invalidateQueries({ queryKey: chatKeys.sessions(wsId) });
    const onVisible = () => {
      if (document.visibilityState === "visible") refresh();
    };
    window.addEventListener("focus", refresh);
    window.addEventListener("online", refresh);
    document.addEventListener("visibilitychange", onVisible);
    const delay = Math.min(Math.max(nextChange - Date.now() + REFRESH_SLACK_MS, REFRESH_MIN_MS), REFRESH_MAX_MS);
    if (retries.current.for !== nextChange) retries.current = { for: nextChange, n: 0 };
    const timer =
      Number.isFinite(nextChange) && retries.current.n < REFRESH_MAX_RETRIES
        ? setTimeout(() => {
            retries.current.n += 1;
            refresh();
            setAttempt((a) => a + 1);
          }, delay)
        : undefined;
    return () => {
      window.removeEventListener("focus", refresh);
      window.removeEventListener("online", refresh);
      document.removeEventListener("visibilitychange", onVisible);
      clearTimeout(timer);
    };
  }, [enabled, qc, wsId, nextChange, attempt]);

}
