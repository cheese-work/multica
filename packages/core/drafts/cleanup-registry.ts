import { defaultStorage } from "../platform/storage";
import type { StorageAdapter } from "../types/storage";
import { abortAll as abortAllUploads } from "./upload-coordinator";

/**
 * Self-registration registry for draft stores, replacing the hand-maintained
 * `WORKSPACE_SCOPED_KEYS` array in `platform/storage-cleanup.ts`.
 *
 * The old model required every new draft store to remember to append its
 * persist key to a list living in a different file; that list drifted and
 * left `multica_comment_drafts`, `multica_quick_create`, `multica_project_draft`,
 * `multica_feedback_draft`, and the chat draft-attachment / restore keys
 * uncleared on logout and workspace deletion (persistence-layer leak), while
 * the in-memory Zustand singletons kept a previous user's draft after a
 * client-side logout navigation (memory-layer leak, cross-user on a shared
 * profile).
 *
 * A store registers ONCE at module load via `registerDraftCleanup`. Cleanup
 * then iterates the registry, so adding a draft store can never again silently
 * skip cleanup.
 */
export interface DraftCleanupEntry {
  /**
   * Base persist key, before any workspace-slug suffix. For workspace-scoped
   * stores the real localStorage key is `${storageKey}:${slug}`.
   */
  storageKey: string;
  /**
   * True when persisted through `createWorkspaceAwareStorage` (key suffixed
   * with the active slug). False for globally-namespaced keys.
   */
  workspaceScoped: boolean;
  /**
   * Reset this store's in-memory state to empty. Called on logout so the
   * Zustand singleton cannot surface a previous user's draft after a
   * client-side navigation (no full page reload clears module singletons).
   */
  resetInMemory: () => void;
  /**
   * Set by stores whose persisted keys carry the session token
   * (`<key>#<token>`): the backing storage to sweep for dead-token keys.
   */
  sessioned?: StorageAdapter;
}

const entries = new Map<string, DraftCleanupEntry>();

/**
 * Draft sessions. A keyed store persists under `<key>#<session token>`, so a
 * store reads only keys of the current session. Logout and workspace delete
 * first end the session (a fresh random id in shared storage), then remove the
 * stored drafts. Between those two steps, and for a write already in flight,
 * old data may sit under an old-token key: no reader accepts it, it cannot
 * overwrite the new session's key, and `sweepDeadDrafts` removes it.
 *
 * Delayed writes (editor flush on unmount, upload settling) carry the token
 * they were created under and are dropped once it is stale. A plain workspace
 * switch ends nothing. Cleanup never removes the id keys.
 */
export const DRAFT_SESSION_KEY = "multica_draft_session";
const workspaceKey = (slug: string) => `${DRAFT_SESSION_KEY}:${slug}`;

/** Key suffix for a token: none before any session ended (the plain, older key). */
export const sessionKeySuffix = (token: string): string => (token === NO_DRAFT_SESSION ? "" : `#${token}`);

/** Token before any session ended on this profile; also what unstamped (older) payloads count as. */
export const NO_DRAFT_SESSION = "|";

// Fallback when storage is unavailable: the ids this tab minted still guard this tab.
const memoryIds = new Map<string, string>();
const readId = (key: string): string => {
  try {
    const stored = defaultStorage.getItem(key);
    if (stored) return stored;
  } catch {
    // see memoryIds
  }
  return memoryIds.get(key) ?? "";
};
const endSession = (key: string) => {
  const id = globalThis.crypto?.randomUUID?.() ?? `${Date.now().toString(36)}${Math.random().toString(36).slice(2)}`;
  memoryIds.set(key, id);
  try {
    defaultStorage.setItem(key, id);
  } catch {
    // see memoryIds
  }
};

/** Where a draft write belongs: its workspace and the session token then. */
export interface DraftOrigin {
  slug: string | null;
  session: string;
}

/** Changes whenever logout or a delete of `slug` ends the session of its drafts, in this tab or another. */
export function draftSessionToken(slug: string | null): string {
  return `${readId(DRAFT_SESSION_KEY)}|${slug ? readId(workspaceKey(slug)) : ""}`;
}

let sessionGeneration = 0;
const workspaceGenerations = new Map<string, number>();

/** The part of the session that only this tab's own cleanup moves; tells a mounted hook to rebind. */
export function localDraftGeneration(slug: string | null): number {
  return sessionGeneration + (slug ? (workspaceGenerations.get(slug) ?? 0) : 0);
}
export function currentDraftOrigin(slug: string | null): DraftOrigin {
  return { slug, session: draftSessionToken(slug) };
}

/** Register a draft store for workspace/logout cleanup. Idempotent per key. */
export function registerDraftCleanup(entry: DraftCleanupEntry): void {
  entries.set(entry.storageKey, entry);
}

/**
 * Remove every registered draft's persisted storage for one workspace slug.
 * Called on workspace delete/leave (per slug) and logout (per workspace the
 * user belonged to). Globally-namespaced draft keys are removed regardless of
 * slug — passing any slug clears them once.
 */
export function clearRegisteredWorkspaceDrafts(
  adapter: StorageAdapter,
  slug: string,
): void {
  workspaceGenerations.set(slug, (workspaceGenerations.get(slug) ?? 0) + 1);
  endSession(workspaceKey(slug)); // before the removals: see "Draft sessions"
  const stored = adapter.keys?.() ?? [];
  for (const entry of entries.values()) {
    const name = entry.workspaceScoped ? `${entry.storageKey}:${slug}` : entry.storageKey;
    adapter.removeItem(name);
    // Session-keyed copies (`<name>#<token>`), whatever token they carry.
    for (const key of stored) if (key.startsWith(`${name}#`)) adapter.removeItem(key);
  }
  sweepDeadDrafts();
}

/**
 * Remove every session-keyed draft key whose token is not the current one.
 * A write that was already in flight when a session ended lands under the old
 * token, so it can neither replace the new session's draft nor be read; this
 * removes it from storage. Runs at logout, workspace delete, store creation
 * and on every `storage` event (another tab wrote a key).
 */
export function sweepDeadDrafts(): void {
  for (const entry of entries.values()) {
    const adapter = entry.sessioned;
    if (!adapter?.keys) continue;
    const base = entry.workspaceScoped ? `${entry.storageKey}:` : entry.storageKey;
    for (const key of adapter.keys()) {
      if (!key.startsWith(base)) continue;
      const rest = key.slice(base.length);
      const hash = rest.indexOf("#");
      // Unscoped: `base` or `base#<token>`; a longer key belongs to another store.
      if (!entry.workspaceScoped && rest !== "" && hash !== 0) continue;
      const slug = entry.workspaceScoped ? (hash < 0 ? rest : rest.slice(0, hash)) : null;
      const name = entry.workspaceScoped ? `${base}${slug}` : base;
      if (key !== name + sessionKeySuffix(draftSessionToken(slug))) adapter.removeItem(key);
    }
  }
}

if (typeof window !== "undefined") window.addEventListener("storage", sweepDeadDrafts);

/**
 * Reset all registered draft stores' in-memory state. Called on logout before
 * auth is cleared, so no draft survives into the next login on the same tab.
 */
export function resetAllRegisteredDrafts(): void {
  sessionGeneration += 1;
  endSession(DRAFT_SESSION_KEY);
  // Abort in-flight uploads BEFORE clearing drafts: an upload that settles
  // after the draft is wiped must not resurrect a placeholder, and its bytes
  // must never bind an attachment under the next session (MUL-5181).
  abortAllUploads();
  for (const entry of entries.values()) {
    entry.resetInMemory();
  }
  sweepDeadDrafts();
}

/**
 * Persist keys of every registered draft store, split by how they are
 * namespaced. Used to sweep drafts for workspaces whose slugs this process
 * never learned — a cold start that is rejected at the identity probe has no
 * workspace list to enumerate.
 */
export function registeredDraftKeys(): {
  workspaceScoped: string[];
  global: string[];
} {
  const workspaceScoped: string[] = [];
  const global: string[] = [];
  for (const entry of entries.values()) {
    (entry.workspaceScoped ? workspaceScoped : global).push(entry.storageKey);
  }
  return { workspaceScoped, global };
}

/** Test-only: drop all registrations. */
export function __clearDraftCleanupRegistryForTest(): void {
  entries.clear();
}

/** Test-only: inspect registered keys. */
export function __getRegisteredDraftKeysForTest(): string[] {
  return [...entries.keys()];
}
