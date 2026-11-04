/**
 * Per-session chat drafts. Kept in memory for synchronous reads by the
 * composer, and mirrored to disk (debounced) through `draft-persistence` so
 * unsent text survives backgrounding kills and app cold starts. Disk entries
 * expire after 30 days, are scoped per workspace, and are deleted on logout.
 *
 * Disk is empty until `hydrate(workspaceId)` runs (from the workspace layout);
 * writes made before that stay in memory only.
 *
 * Key conventions:
 *   - Real session id (UUID) for any existing session
 *   - DRAFT_NEW_SESSION sentinel for the not-yet-created new-chat input
 */
import { create } from "zustand";
import {
  draftPersistence,
  registerDraftCleanup,
} from "../draft-persistence";

export const DRAFT_NEW_SESSION = "__new__";

const DISK_PREFIX = "chat:";

interface ChatDraftsState {
  drafts: Record<string, string>;
  setDraft: (sessionId: string, text: string) => void;
  clearDraft: (sessionId: string) => void;
  /** Move the `__new__` draft onto a freshly-created session id without
   *  the user seeing an empty input on the first frame after send. */
  promoteNewDraft: (newSessionId: string) => void;
  /** Switch to a workspace: drop the previous workspace's drafts from memory
   *  and load this workspace's drafts from disk. */
  hydrate: (workspaceId: string) => Promise<void>;
  /** Empty memory and detach from disk (logout). */
  reset: () => void;
}

// Workspace whose drafts are on screen; null until hydrate() runs.
let scope: string | null = null;
// Session ids written or cleared since the current hydrate() began. A disk
// read that resolves later must not overwrite or resurrect them.
let touchedDuringHydrate = new Set<string>();
let hydrating = false;

function persist(sessionId: string, text: string | null) {
  if (hydrating) touchedDuringHydrate.add(sessionId);
  if (scope === null) return;
  if (text === null) draftPersistence.remove(scope, DISK_PREFIX + sessionId);
  else draftPersistence.save(scope, DISK_PREFIX + sessionId, text);
}

export const useChatDraftsStore = create<ChatDraftsState>((set, get) => ({
  drafts: {},
  setDraft: (sessionId, text) => {
    const current = get().drafts;
    // Skip the set when the value is identical — Zustand would still emit
    // a notification and trigger a re-render of every selector subscriber.
    if (current[sessionId] === text) return;
    if (text === "") {
      // Empty input == no draft; prune so we don't accumulate dead keys.
      if (!(sessionId in current)) return;
      const next = { ...current };
      delete next[sessionId];
      set({ drafts: next });
      persist(sessionId, null);
      return;
    }
    set({ drafts: { ...current, [sessionId]: text } });
    persist(sessionId, text);
  },
  clearDraft: (sessionId) => {
    const current = get().drafts;
    if (!(sessionId in current)) return;
    const next = { ...current };
    delete next[sessionId];
    set({ drafts: next });
    persist(sessionId, null);
  },
  promoteNewDraft: (newSessionId) => {
    const current = get().drafts;
    const pending = current[DRAFT_NEW_SESSION];
    if (!pending) return;
    const next = { ...current, [newSessionId]: pending };
    delete next[DRAFT_NEW_SESSION];
    set({ drafts: next });
    persist(newSessionId, pending);
    persist(DRAFT_NEW_SESSION, null);
  },
  hydrate: async (workspaceId) => {
    if (scope === workspaceId) return;
    // Pending writes carry their own scope, so flushing is safe after the
    // switch; do it first so the previous workspace's last keystrokes land.
    void draftPersistence.flush();
    scope = workspaceId;
    touchedDuringHydrate = new Set();
    hydrating = true;
    set({ drafts: {} });
    void draftPersistence.sweepExpired();
    const onDisk = await draftPersistence.loadAll<string>(
      workspaceId,
      DISK_PREFIX,
    );
    // A newer hydrate() or a logout reset won while we were reading.
    if (scope !== workspaceId) return;
    hydrating = false;
    const restored: Record<string, string> = {};
    for (const [key, text] of Object.entries(onDisk)) {
      const sessionId = key.slice(DISK_PREFIX.length);
      if (typeof text === "string" && !touchedDuringHydrate.has(sessionId)) {
        restored[sessionId] = text;
      }
    }
    set({ drafts: { ...restored, ...get().drafts } });
  },
  reset: () => {
    scope = null;
    hydrating = false;
    touchedDuringHydrate = new Set();
    set({ drafts: {} });
  },
}));

registerDraftCleanup(() => useChatDraftsStore.getState().reset());
