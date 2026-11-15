import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { useWorkspaceId } from "../hooks";
import { chatKeys } from "./queries";
import { snoozableKeys } from "./attention";

// Attention actions never patch the cache: the server owns the obligation state, so each
// action waits for the server and refetches the session list on settle.
function useInvalidateSessions() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return () => qc.invalidateQueries({ queryKey: chatKeys.sessions(wsId) });
}

/** Xong: ack `turn:` keys and resolve `item:` keys. `decision:` keys are dropped (T3 → 422). */
export function useChatAttentionXong() {
  const invalidate = useInvalidateSessions();
  return useMutation({
    mutationFn: async ({ sessionId, keys }: { sessionId: string; keys: readonly string[] }) => {
      const sendable = snoozableKeys(keys);
      if (sendable.length === 0) return null;
      return api.chatAttentionXong(sessionId, sendable);
    },
    onSettled: invalidate,
  });
}

/** Để sau: snooze every displayed key, decisions included (presentation only, T5). */
export function useChatAttentionLater() {
  const invalidate = useInvalidateSessions();
  return useMutation({
    mutationFn: ({
      sessionId,
      keys,
      durationSeconds,
      requestId,
    }: {
      sessionId: string;
      keys: readonly string[];
      durationSeconds?: number;
      requestId: string;
    }) => api.chatAttentionLater(sessionId, [...keys], { durationSeconds, requestId }),
    onSettled: invalidate,
  });
}

/** Xong tất cả: one batch over every displayed key; the server skips decision keys (T4). */
export function useChatAttentionXongAll() {
  const invalidate = useInvalidateSessions();
  return useMutation({
    mutationFn: (keys: readonly string[]) => api.chatAttentionXongAll([...keys]),
    onSettled: invalidate,
  });
}

export function useChatAttentionUndoXongAll() {
  const invalidate = useInvalidateSessions();
  return useMutation({
    mutationFn: (batchId: string) => api.chatAttentionUndoXongAll(batchId),
    onSettled: invalidate,
  });
}
