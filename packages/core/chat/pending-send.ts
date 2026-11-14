import { create } from "zustand";
import { createJSONStorage, persist } from "zustand/middleware";
import { api } from "../api";
import { ApiError } from "../api/client";
import { registerDraftCleanup } from "../drafts/cleanup-registry";
import { defaultStorage } from "../platform/storage";
import { createSafeId } from "../utils";

/**
 * Pending chat send (B4 / A22): the `client_message_id` of a send whose answer
 * has not been seen, kept with its session across reload so a retry reuses the
 * id and the server stores one message.
 */
export type SendFailure = "rejected" | "unknown";

/**
 * A 4xx (409 included) is a definite rejection: nothing was stored, keep the
 * draft and mint a new id next time. A timeout, network error, 408 or 5xx may
 * have been stored, so the delivery is unknown and the retry keeps the id.
 */
export function classifySendFailure(err: unknown): SendFailure {
  if (err instanceof ApiError && err.status >= 400 && err.status < 500 && err.status !== 408) {
    return "rejected";
  }
  return "unknown";
}

export interface PendingSend {
  clientMessageId: string;
  content: string;
  status: "sending" | "unknown";
}

interface PendingSendState {
  bySession: Record<string, PendingSend>;
  /** Id for sending `content` in `sessionId`: the pending one when the text is unchanged. */
  begin: (sessionId: string, content: string) => string;
  markUnknown: (sessionId: string) => void;
  /** Delivered or definitely rejected: forget the id. */
  settle: (sessionId: string) => void;
}

const STORAGE_KEY = "multica_chat_pending_send";

export const usePendingSendStore = create<PendingSendState>()(
  persist(
    (set, get) => ({
      bySession: {},
      begin: (sessionId, content) => {
        const existing = get().bySession[sessionId];
        const clientMessageId =
          existing && existing.content === content ? existing.clientMessageId : createSafeId();
        set((s) => ({
          bySession: { ...s.bySession, [sessionId]: { clientMessageId, content, status: "sending" } },
        }));
        return clientMessageId;
      },
      markUnknown: (sessionId) =>
        set((s) => {
          const entry = s.bySession[sessionId];
          if (!entry) return s;
          return { bySession: { ...s.bySession, [sessionId]: { ...entry, status: "unknown" } } };
        }),
      settle: (sessionId) =>
        set((s) => {
          if (!(sessionId in s.bySession)) return s;
          const { [sessionId]: _gone, ...rest } = s.bySession;
          return { bySession: rest };
        }),
    }),
    {
      name: STORAGE_KEY,
      storage: createJSONStorage(() => defaultStorage),
      partialize: (s) => ({ bySession: s.bySession }),
    },
  ),
);

registerDraftCleanup({
  storageKey: STORAGE_KEY,
  workspaceScoped: false,
  resetInMemory: () => usePendingSendStore.setState({ bySession: {} }),
});

/**
 * Send with the pending `client_message_id` for this session and text. A
 * success or a definite rejection forgets the id; any other failure keeps it
 * (status "unknown") so the retry cannot store a second message.
 */
export async function sendWithPendingId(
  sessionId: string,
  content: string,
  opts: { attachmentIds?: string[]; decisionRequestId?: string; askIds?: string[] } = {},
) {
  const pending = usePendingSendStore.getState();
  const clientMessageId = pending.begin(sessionId, content);
  try {
    const result = await api.sendChatMessage(
      sessionId,
      content,
      opts.attachmentIds,
      opts.decisionRequestId,
      { clientMessageId, askIds: opts.askIds },
    );
    pending.settle(sessionId);
    return result;
  } catch (err) {
    if (classifySendFailure(err) === "rejected") pending.settle(sessionId);
    else pending.markUnknown(sessionId);
    throw err;
  }
}
