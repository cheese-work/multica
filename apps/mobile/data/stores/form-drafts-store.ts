/**
 * Unsent input of the issue/project create and edit screens. Kept in memory
 * for synchronous reads, mirrored to disk (debounced) through
 * `draft-persistence` so a half-edited title or description survives closing
 * the modal, a route change and an app cold start. Disk entries expire after
 * 30 days, are scoped per user + workspace, and are deleted on logout.
 *
 * One entry per surface, keyed by `issueEditKey(id)`, `projectEditKey(id)`,
 * `NEW_ISSUE_KEY` or `NEW_PROJECT_KEY`. A screen clears its entry only on a
 * successful save/submit or an explicit discard; a failed save keeps it.
 *
 * `ready` is false until `hydrate(scope)` has read the disk. Screens wait for
 * it before seeding their form, otherwise a cold start would seed from the
 * server and overwrite the saved draft on the next write.
 */
import { create } from "zustand";
import { draftPersistence, registerDraftCleanup } from "../draft-persistence";

export const NEW_ISSUE_KEY = "form:new-issue";
export const NEW_PROJECT_KEY = "form:new-project";
export const issueEditKey = (id: string) => `form:issue-edit:${id}`;
export const projectEditKey = (id: string) => `form:project-edit:${id}`;

const DISK_PREFIX = "form:";

/** Title/description (and icon for projects) typed into an edit screen. */
export interface EditDraft {
  title: string;
  description: string;
  icon?: string;
}

interface FormDraftsState {
  drafts: Record<string, unknown>;
  /** Scope (user + workspace) the drafts belong to; null before `hydrate`. */
  scopeId: string | null;
  /** True once the disk of `scopeId` has been merged into memory. */
  ready: boolean;
  /** Keys whose screen has applied its saved draft (see `useRestoredFormDraft`). */
  restored: Record<string, boolean>;
  markRestored: (key: string, value: boolean) => void;
  setDraft: (key: string, value: unknown) => void;
  clearDraft: (key: string) => void;
  /** Switch to a scope: drop the previous scope's drafts from memory and load
   *  this scope's drafts from disk. */
  hydrate: (scope: string) => Promise<void>;
  /** Empty memory and detach from disk (logout). */
  reset: () => void;
}

// Keys written or cleared since the current hydrate() began; a disk read that
// resolves later must not overwrite or resurrect them.
let touched = new Set<string>();

export const useFormDraftsStore = create<FormDraftsState>((set, get) => ({
  drafts: {},
  scopeId: null,
  ready: false,
  restored: {},
  markRestored: (key, value) =>
    set({ restored: { ...get().restored, [key]: value } }),
  setDraft: (key, value) => {
    const { scopeId, drafts } = get();
    touched.add(key);
    set({ drafts: { ...drafts, [key]: value } });
    if (scopeId) draftPersistence.save(scopeId, key, value);
  },
  clearDraft: (key) => {
    const { scopeId, drafts } = get();
    touched.add(key);
    if (key in drafts) {
      const next = { ...drafts };
      delete next[key];
      set({ drafts: next });
    }
    if (scopeId) draftPersistence.remove(scopeId, key);
  },
  hydrate: async (scope) => {
    if (get().scopeId === scope) return;
    // Pending writes carry their own scope; flush so the previous scope's
    // last keystrokes land.
    void draftPersistence.flush();
    touched = new Set();
    set({ drafts: {}, scopeId: scope, ready: false, restored: {} });
    void draftPersistence.sweepExpired();
    const onDisk = await draftPersistence.loadAll<unknown>(scope, DISK_PREFIX);
    // A newer hydrate() or a logout reset won while we were reading.
    if (get().scopeId !== scope) return;
    const restored: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(onDisk)) {
      if (!touched.has(key)) restored[key] = value;
    }
    set({ drafts: { ...restored, ...get().drafts }, ready: true });
  },
  reset: () => {
    touched = new Set();
    set({ drafts: {}, scopeId: null, ready: false, restored: {} });
  },
}));

registerDraftCleanup(() => useFormDraftsStore.getState().reset());

/** The saved draft for `key` once the disk has loaded, else undefined. */
export function readFormDraft<T>(key: string): T | undefined {
  const { ready, drafts } = useFormDraftsStore.getState();
  return ready ? (drafts[key] as T | undefined) : undefined;
}
