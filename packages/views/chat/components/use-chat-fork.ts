"use client";

import { useRef } from "react";
import { clientErrorMessage } from "@multica/core/api";
import { useChatStore } from "@multica/core/chat";
import { useForkChatSession } from "@multica/core/chat/mutations";
import type { ChatSession } from "@multica/core/types";
import { useT } from "../../i18n";

const newIdempotencyKey = () =>
  globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`;

export type ForkResult = { ok: true } | { ok: false; error: string };

/**
 * Handler for the composer's `/fork`. The server copies the DM; this hook owns
 * the idempotency key so retrying the same text and attachments in the same DM
 * replays the request instead of forking twice. The key lives in memory only: it
 * is retired on success or as soon as the DM, the text or the attachments change.
 */
export function useChatFork(
  session: ChatSession | null | undefined,
  onOpenSession: (child: ChatSession) => void,
) {
  const { t } = useT("chat");
  const forkSession = useForkChatSession();
  const keyRef = useRef<{ sessionId: string; message: string; attachments: string; key: string } | null>(null);

  const handleFork = async (message: string, attachmentIds: string[] = []): Promise<ForkResult> => {
    const unconfirmed = { ok: false, error: t(($) => $.input.fork_unconfirmed) } as const;
    if (!session) return unconfirmed;
    const sessionId = session.id;
    let entry = keyRef.current;
    const attachments = [...attachmentIds].sort().join(",");
    if (entry?.sessionId !== sessionId || entry.message !== message || entry.attachments !== attachments) {
      entry = { sessionId, message, attachments, key: newIdempotencyKey() };
      keyRef.current = entry;
    }
    try {
      const response = await forkSession.mutateAsync({
        sessionId,
        idempotencyKey: entry.key,
        message,
        attachmentIds,
      });
      // Malformed success body: the fork may have happened, so keep the key.
      if (!response.session.id) return unconfirmed;
      keyRef.current = null;
      // Always open the child (idle when there is no message), but only if the
      // person is still looking at the source DM.
      if (useChatStore.getState().activeSessionId === sessionId) {
        onOpenSession(response.session);
      }
      return { ok: true };
    } catch (err) {
      const error = clientErrorMessage(err);
      return error ? { ok: false, error } : unconfirmed;
    }
  };

  return { handleFork };
}
