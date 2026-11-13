/**
 * DM attention actions (Xong, Để sau). No optimistic patch: the server owns
 * the obligation state, so each action waits for the server and refetches the
 * session list on settle. Mirrors web's `core/chat/attention-mutations`
 * without importing it (mobile owns its data layer).
 */
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { laterSession, xongSession } from "@/data/chat-attention-actions";
import { chatKeys } from "@/data/queries/chat";
import { useWorkspaceStore } from "@/data/workspace-store";

function useInvalidateSessions() {
  const qc = useQueryClient();
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  return () => qc.invalidateQueries({ queryKey: chatKeys.sessions(wsId) });
}

export function useChatAttentionXong() {
  const invalidate = useInvalidateSessions();
  return useMutation({
    mutationFn: ({ sessionId, keys }: { sessionId: string; keys: readonly string[] }) =>
      xongSession(sessionId, keys),
    onSettled: invalidate,
  });
}

export function useChatAttentionLater() {
  const invalidate = useInvalidateSessions();
  return useMutation({
    mutationFn: ({
      sessionId,
      keys,
      requestId,
    }: {
      sessionId: string;
      keys: readonly string[];
      requestId: string;
    }) => laterSession(sessionId, keys, requestId),
    onSettled: invalidate,
  });
}
