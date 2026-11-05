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
}

const entries = new Map<string, DraftCleanupEntry>();

/**
 * Cleanup generations. Logout and workspace delete bump them; a delayed draft
 * write (editor flush on unmount, upload settling) carries the generation it
 * was created under and is dropped once that is stale, so it cannot recreate a
 * draft cleanup just removed. A plain workspace switch bumps nothing.
 */
let sessionGeneration = 0;
const workspaceGenerations = new Map<string, number>();

/** Where a draft write belongs: its workspace and the cleanup generation then. */
export interface DraftOrigin {
  slug: string | null;
  generation: number;
}

/** Strictly increases whenever logout or a delete of `slug` clears its drafts. */
export function getDraftGeneration(slug: string | null): number {
  return sessionGeneration + (slug ? (workspaceGenerations.get(slug) ?? 0) : 0);
}

export function currentDraftOrigin(slug: string | null): DraftOrigin {
  return { slug, generation: getDraftGeneration(slug) };
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
  for (const entry of entries.values()) {
    if (entry.workspaceScoped) {
      adapter.removeItem(`${entry.storageKey}:${slug}`);
    } else {
      adapter.removeItem(entry.storageKey);
    }
  }
}

/**
 * Reset all registered draft stores' in-memory state. Called on logout before
 * auth is cleared, so no draft survives into the next login on the same tab.
 */
export function resetAllRegisteredDrafts(): void {
  sessionGeneration += 1;
  // Abort in-flight uploads BEFORE clearing drafts: an upload that settles
  // after the draft is wiped must not resurrect a placeholder, and its bytes
  // must never bind an attachment under the next session (MUL-5181).
  abortAllUploads();
  for (const entry of entries.values()) {
    entry.resetInMemory();
  }
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
