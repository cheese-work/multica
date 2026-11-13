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
  registerScopeCleanup,
} from "../draft-persistence";

export const DRAFT_NEW_SESSION = "__new__";

const DISK_PREFIX = "chat:";

interface ChatDraftsState {
  drafts: Record<string, string>;
  /** Scope (user + workspace) the drafts belong to; null before `hydrate`. */
  scopeId: string | null;
  /** `forScope` is the scope the caller rendered in. A write for a scope that
   *  was left since (a send settling after a workspace switch) is dropped. */
  setDraft: (sessionId: string, text: string, forScope?: string | null) => void;
  clearDraft: (sessionId: string) => void;
  /** Move the `__new__` draft onto a freshly-created session id without
   *  the user seeing an empty input on the first frame after send.
   *  `forScope` is the scope the send started in; session creation is async,
   *  so a scope that was left since only repoints its own open sends. */
  promoteNewDraft: (newSessionId: string, forScope?: string | null) => void;
  /** Switch to a workspace: drop the previous workspace's drafts from memory
   *  and load this workspace's drafts from disk. */
  hydrate: (workspaceId: string) => Promise<void>;
  /** The composer is about to empty its input to submit `text`. Keep that
   *  text on disk until `finishSend`; returns the token to settle with. */
  beginSend: (sessionId: string, text: string) => string;
  /** The send settled. Success drops the submitted text from disk; failure
   *  leaves whatever the input holds now (the composer restores it). */
  finishSend: (token: string, ok: boolean) => void;
  /** A workspace was deleted: if it is the current scope, detach from it so
   *  a pending load and late writes cannot bring its drafts back. */
  dropScope: (scope: string) => void;
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
// nothing. Each entry is bound to the scope (user + workspace) it started in:
// after a workspace switch it must neither show up in the new scope nor be
// settled against it.
interface InflightSend {
  scope: string | null;
  sessionId: string;
  text: string;
  /** Input text of the session at the moment its scope was left. */
  parkedText: string;
}
const inflight = new Map<string, InflightSend>();
let nextToken = 0;

/** Disk value: a plain string, or `{ text, sending }` while a send is open. */
interface DiskDraft {
  text: string;
  sending: string;
}

function pendingText(forScope: string | null, sessionId: string): string {
  let joined = "";
  for (const entry of inflight.values()) {
    if (entry.scope === forScope && entry.sessionId === sessionId) {
      joined = joined ? `${joined}\n${entry.text}` : entry.text;
    }
  }
  return joined;
}

function writeDisk(forScope: string, sessionId: string, text: string | null) {
  const key = DISK_PREFIX + sessionId;
  const sending = pendingText(forScope, sessionId);
  if (sending) {
    const value: DiskDraft = { text: text ?? "", sending };
    draftPersistence.save(forScope, key, value);
  } else if (text === null) draftPersistence.remove(forScope, key);
  else draftPersistence.save(forScope, key, text);
}

function persist(sessionId: string, text: string | null) {
  if (hydrating) touchedDuringHydrate.add(sessionId);
  if (scope === null) return;
  writeDisk(scope, sessionId, text);
}

function restoredText(
  value: unknown,
  sessionId: string,
  forScope: string,
): string | null {
  if (typeof value === "string") return value;
  if (typeof value !== "object" || value === null) return null;
  const { text, sending } = value as Partial<DiskDraft>;
  if (typeof text !== "string" || typeof sending !== "string") return null;
  // A send of this process is still open: its text is tracked in memory, so
  // showing it in the input as well would duplicate it.
  if (pendingText(forScope, sessionId)) return text;
  return mergeRestoredText(sending, text);
}

export const useChatDraftsStore = create<ChatDraftsState>((set, get) => ({
  drafts: {},
  scopeId: null,
  beginSend: (sessionId, text) => {
    const token = String(++nextToken);
    inflight.set(token, { scope, sessionId, text, parkedText: "" });
    persist(sessionId, get().drafts[sessionId] ?? null);
    return token;
  },
  finishSend: (token, ok) => {
    const entry = inflight.get(token);
    if (!entry) return;
    inflight.delete(token);
    if (entry.scope === null) return;
    if (entry.scope === scope) {
      persist(entry.sessionId, get().drafts[entry.sessionId] ?? null);
      return;
    }
    // The user left this scope mid-send; settle against the scope's own disk
    // entry using the input text it had when it was left. A failed send keeps
    // its text as a plain draft.
    const rest = ok
      ? entry.parkedText
      : mergeRestoredText(entry.text, entry.parkedText);
    writeDisk(entry.scope, entry.sessionId, rest || null);
  },
  setDraft: (sessionId, text, forScope) => {
    if (forScope !== undefined && forScope !== scope) return;
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
  promoteNewDraft: (newSessionId, forScope) => {
    const origin = forScope === undefined ? scope : forScope;
    // An open send follows the draft onto the created session.
    let repointed: InflightSend | null = null;
    for (const entry of inflight.values()) {
      if (entry.scope === origin && entry.sessionId === DRAFT_NEW_SESSION) {
        entry.sessionId = newSessionId;
        repointed = entry;
      }
    }
    if (origin !== scope) {
      // The scope was left while the session was being created: only its
      // disk entry moves, and the current scope's drafts stay untouched.
      if (repointed && origin !== null) {
        writeDisk(origin, newSessionId, repointed.parkedText || null);
        writeDisk(origin, DRAFT_NEW_SESSION, null);
      }
      return;
    }
    const current = get().drafts;
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
    // Park open sends of the scope being left (see InflightSend).
    for (const entry of inflight.values()) {
      if (entry.scope === scope) {
        entry.parkedText = get().drafts[entry.sessionId] ?? "";
      }
    }
    scope = workspaceId;
    touchedDuringHydrate = new Set();
    hydrating = true;
    set({ drafts: {}, scopeId: workspaceId });
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
      const text = restoredText(value, sessionId, workspaceId);
      if (text && !touchedDuringHydrate.has(sessionId)) {
        restored[sessionId] = text;
      }
    }
    set({ drafts: { ...restored, ...get().drafts } });
  },
  dropScope: (deleted) => {
    for (const [token, entry] of inflight) {
      if (entry.scope === deleted) inflight.delete(token);
    }
    if (scope === deleted) get().reset();
  },
  reset: () => {
    scope = null;
    hydrating = false;
    touchedDuringHydrate = new Set();
    inflight.clear();
    set({ drafts: {}, scopeId: null });
  },
}));

registerDraftCleanup(() => useChatDraftsStore.getState().reset());
registerScopeCleanup((deleted) =>
  useChatDraftsStore.getState().dropScope(deleted),
);
