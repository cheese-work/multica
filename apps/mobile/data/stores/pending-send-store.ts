/**
 * Pending chat send (B4 / A22): the `client_message_id` of a send whose answer
 * has not been seen, kept per session and mirrored to disk so a retry, even
 * after an app restart, reuses the id and the server stores one message.
 *
 * Mobile owns this store; it mirrors the shared rule without importing core
 * runtime stores. Disk is empty until `hydrate(scope)` runs (use-draft-lifecycle).
 */
import { create } from "zustand";
import { api } from "@/data/api";
import { createIdempotencyKey } from "../../lib/request-id";
import { draftPersistence, registerDraftCleanup } from "../draft-persistence";

export type SendFailure = "rejected" | "unknown";

/**
 * A 4xx other than 408 is a definite rejection: nothing was stored, keep the
 * draft and mint a new id next time. A timeout, network error, 408 or 5xx may
 * have been stored, so delivery is unknown and the retry keeps the id.
 */
export function classifySendFailure(err: unknown): SendFailure {
  const status = (err as { status?: unknown } | null)?.status;
  if (typeof status === "number" && status >= 400 && status < 500 && status !== 408) {
    return "rejected";
  }
  return "unknown";
}

export interface PendingSend {
  clientMessageId: string;
  content: string;
}

const DISK_PREFIX = "pending-send:";

interface PendingSendState {
  scopeId: string | null;
  bySession: Record<string, PendingSend>;
  hydrate: (scope: string) => Promise<void>;
  /** Id for sending `content` in `sessionId`: the pending one when the text is unchanged. */
  begin: (sessionId: string, content: string) => string;
  /** Delivered or definitely rejected: forget the id. */
  settle: (sessionId: string) => void;
  reset: () => void;
}

function isPendingSend(v: unknown): v is PendingSend {
  const p = v as Partial<PendingSend> | null;
  return typeof p?.clientMessageId === "string" && typeof p.content === "string";
}

export const usePendingSendStore = create<PendingSendState>((set, get) => ({
  scopeId: null,
  bySession: {},
  hydrate: async (scope) => {
    if (get().scopeId === scope) return;
    set({ scopeId: scope, bySession: {} });
    const onDisk = await draftPersistence.loadAll<unknown>(scope, DISK_PREFIX);
    if (get().scopeId !== scope) return;
    const restored: Record<string, PendingSend> = {};
    for (const [key, value] of Object.entries(onDisk)) {
      if (isPendingSend(value)) restored[key.slice(DISK_PREFIX.length)] = value;
    }
    set({ bySession: { ...restored, ...get().bySession } });
  },
  begin: (sessionId, content) => {
    const existing = get().bySession[sessionId];
    if (existing && existing.content === content) return existing.clientMessageId;
    const entry: PendingSend = { clientMessageId: createIdempotencyKey(), content };
    set({ bySession: { ...get().bySession, [sessionId]: entry } });
    const { scopeId } = get();
    if (scopeId) draftPersistence.save(scopeId, DISK_PREFIX + sessionId, entry);
    return entry.clientMessageId;
  },
  settle: (sessionId) => {
    const { bySession, scopeId } = get();
    if (!(sessionId in bySession)) return;
    const { [sessionId]: _gone, ...rest } = bySession;
    set({ bySession: rest });
    if (scopeId) draftPersistence.remove(scopeId, DISK_PREFIX + sessionId);
  },
  reset: () => set({ scopeId: null, bySession: {} }),
}));

registerDraftCleanup(() => usePendingSendStore.getState().reset());

/**
 * Send with the pending `client_message_id` for this session and text. A
 * success or a definite rejection forgets the id; any other failure keeps it
 * so the retry cannot store a second message.
 */
export async function sendWithPendingId(
  sessionId: string,
  content: string,
  opts: { attachmentIds?: string[]; askIds?: string[] } = {},
) {
  const pending = usePendingSendStore.getState();
  const clientMessageId = pending.begin(sessionId, content);
  try {
    const result = await api.sendChatMessage(sessionId, content, {
      attachmentIds: opts.attachmentIds,
      clientMessageId,
      askIds: opts.askIds,
    });
    pending.settle(sessionId);
    return result;
  } catch (err) {
    if (classifySendFailure(err) === "rejected") pending.settle(sessionId);
    throw err;
  }
}
