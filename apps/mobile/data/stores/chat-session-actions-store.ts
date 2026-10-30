import { useEffect, useRef } from "react";
import { create } from "zustand";

export type ChatSessionActionKind = "archive" | "unarchive";

export interface ArchiveUndoToast {
  sessionId: string;
  title: string;
  expiresAt: number;
}

const UNDO_TOAST_DURATION_MS = 5_000;
let undoToastTimer: ReturnType<typeof setTimeout> | null = null;

function clearUndoTimer() {
  if (undoToastTimer) {
    clearTimeout(undoToastTimer);
    undoToastTimer = null;
  }
}

interface ChatSessionActionsState {
  pendingBySession: Record<string, ChatSessionActionKind>;
  undoToast: ArchiveUndoToast | null;
  beginSessionAction: (sessionId: string, action: ChatSessionActionKind) => boolean;
  endSessionAction: (sessionId: string) => void;
  showArchiveUndo: (payload: { sessionId: string; title: string }) => void;
  dismissArchiveUndo: () => void;
  reset: () => void;
}

export const useChatSessionActionsStore = create<ChatSessionActionsState>((set, get) => ({
  pendingBySession: {},
  undoToast: null,

  beginSessionAction: (sessionId, action) => {
    const current = get().pendingBySession[sessionId];
    if (current) return false;
    set((state) => ({
      pendingBySession: { ...state.pendingBySession, [sessionId]: action },
    }));
    return true;
  },

  endSessionAction: (sessionId) => {
    set((state) => {
      if (!(sessionId in state.pendingBySession)) return state;
      const next = { ...state.pendingBySession };
      delete next[sessionId];
      return { pendingBySession: next };
    });
  },

  showArchiveUndo: ({ sessionId, title }) => {
    clearUndoTimer();
    const expiresAt = Date.now() + UNDO_TOAST_DURATION_MS;
    set({
      undoToast: { sessionId, title, expiresAt },
    });
    undoToastTimer = setTimeout(() => {
      undoToastTimer = null;
      set((state) =>
        state.undoToast?.sessionId === sessionId && state.undoToast?.expiresAt === expiresAt
          ? { undoToast: null }
          : state,
      );
    }, UNDO_TOAST_DURATION_MS);
  },

  dismissArchiveUndo: () => {
    clearUndoTimer();
    set({ undoToast: null });
  },

  reset: () => {
    clearUndoTimer();
    set({ pendingBySession: {}, undoToast: null });
  },
}));

export function useChatSessionActionsResetOnWorkspaceChange(
  wsId: string | null,
) {
  const prevRef = useRef(wsId);
  useEffect(() => {
    if (prevRef.current !== wsId) {
      useChatSessionActionsStore.getState().reset();
      prevRef.current = wsId;
    }
  }, [wsId]);
}
