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
import { mergeRestoredText } from "../../lib/merge-restored-text";
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
  /** The composer is about to empty its input to submit `text`. Keep that
   *  text on disk until `finishSend`; returns the token to settle with. */
  beginSend: (sessionId: string, text: string) => string;
  /** The send settled. Success drops the submitted text from disk; failure
   *  leaves whatever the input holds now (the composer restores it). */
  finishSend: (token: string, ok: boolean) => void;
  /** Empty memory and detach from disk (logout). */
  reset: () => void;
}

// Workspace whose drafts are on screen; null until hydrate() runs.
let scope: string | null = null;
// Session ids written or cleared since the current hydrate() began. A disk
// read that resolves later must not overwrite or resurrect them.
let touchedDuringHydrate = new Set<string>();
let hydrating = false;

// Submitted text whose send has not settled yet. The in-memory draft is empty
// during the send; the disk entry keeps this text so a kill mid-send loses
// nothing.
const inflight = new Map<string, { sessionId: string; text: string }>();
let nextToken = 0;

/** Disk value: a plain string, or `{ text, sending }` while a send is open. */
interface DiskDraft {
  text: string;
  sending: string;
}

function pendingText(sessionId: string): string {
  let joined = "";
  for (const entry of inflight.values()) {
    if (entry.sessionId === sessionId) {
      joined = joined ? `${joined}\n${entry.text}` : entry.text;
    }
  }
  return joined;
}

function persist(sessionId: string, text: string | null) {
  if (hydrating) touchedDuringHydrate.add(sessionId);
  if (scope === null) return;
  const key = DISK_PREFIX + sessionId;
  const sending = pendingText(sessionId);
  if (sending) {
    const value: DiskDraft = { text: text ?? "", sending };
    draftPersistence.save(scope, key, value);
  } else if (text === null) draftPersistence.remove(scope, key);
  else draftPersistence.save(scope, key, text);
}

function restoredText(value: unknown): string | null {
  if (typeof value === "string") return value;
  if (typeof value !== "object" || value === null) return null;
  const { text, sending } = value as Partial<DiskDraft>;
  if (typeof text !== "string" || typeof sending !== "string") return null;
  return mergeRestoredText(sending, text);
}

export const useChatDraftsStore = create<ChatDraftsState>((set, get) => ({
  drafts: {},
  beginSend: (sessionId, text) => {
    const token = String(++nextToken);
    inflight.set(token, { sessionId, text });
    persist(sessionId, get().drafts[sessionId] ?? null);
    return token;
  },
  finishSend: (token, _ok) => {
    const entry = inflight.get(token);
    if (!entry) return;
    inflight.delete(token);
    persist(entry.sessionId, get().drafts[entry.sessionId] ?? null);
  },
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
    // An open send follows the draft onto the created session.
    let repointed = false;
    for (const entry of inflight.values()) {
      if (entry.sessionId === DRAFT_NEW_SESSION) {
        entry.sessionId = newSessionId;
        repointed = true;
      }
    }
    const pending = current[DRAFT_NEW_SESSION];
    if (!pending) {
      if (repointed) {
        persist(newSessionId, current[newSessionId] ?? null);
        persist(DRAFT_NEW_SESSION, null);
      }
      return;
    }
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
    const onDisk = await draftPersistence.loadAll<unknown>(
      workspaceId,
      DISK_PREFIX,
    );
    // A newer hydrate() or a logout reset won while we were reading.
    if (scope !== workspaceId) return;
    hydrating = false;
    const restored: Record<string, string> = {};
    for (const [key, value] of Object.entries(onDisk)) {
      const sessionId = key.slice(DISK_PREFIX.length);
      const text = restoredText(value);
      if (text && !touchedDuringHydrate.has(sessionId)) {
        restored[sessionId] = text;
      }
    }
    set({ drafts: { ...restored, ...get().drafts } });
  },
  reset: () => {
    scope = null;
    hydrating = false;
    touchedDuringHydrate = new Set();
    inflight.clear();
    set({ drafts: {} });
  },
}));

registerDraftCleanup(() => useChatDraftsStore.getState().reset());
