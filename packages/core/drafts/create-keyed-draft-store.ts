import { create, type StoreApi, type UseBoundStore } from "zustand";
import { createJSONStorage, persist } from "zustand/middleware";
import {
  createWorkspaceAwareStorage,
  getCurrentSlug,
  registerForWorkspaceRehydration,
} from "../platform/workspace-storage";
import { defaultStorage } from "../platform/storage";
import type { StorageAdapter } from "../types/storage";
import { getDraftGeneration, registerDraftCleanup, type DraftOrigin } from "./cleanup-registry";

/**
 * Keyed sibling of {@link createDraftStore}: one persisted store holding many
 * drafts, one per entity key (an issue id, a thread id, a dialog target).
 * Surfaces whose input unmounts while it still holds unsent work — virtualized
 * rows, dialogs, route changes, tab close — read and write through this
 * instead of hand-rolling a `Record<key, draft>` store each time.
 *
 * Same wiring as the singleton factory: workspace-scoped persistence,
 * workspace rehydration, `registerDraftCleanup` (logout and workspace delete),
 * and an in-memory reset on logout. Entries older than the TTL, or that
 * `hasMeaningful` rejects, are dropped when the store loads.
 */
export interface KeyedDraftStoreConfig<TData> {
  /** Persist key, before the workspace-slug suffix. Must be globally unique. */
  storageKey: string;
  /** Starting point for a key's first patch. Never mutated or shared. */
  emptyData: TData;
  /** Whether an entry holds recoverable user intent (text, choices, uploads). */
  hasMeaningful: (data: TData) => boolean;
  /** Persist per workspace slug via `createWorkspaceAwareStorage`. Default true. */
  workspaceScoped?: boolean;
  /** Entry lifetime from its last write. Default 30 days. */
  ttlMs?: number;
  /** Backing storage. Default `defaultStorage`; tests inject an in-memory one. */
  storage?: StorageAdapter;
}

export interface KeyedDraftEntry<TData> {
  data: TData;
  updatedAt: number;
}

export interface KeyedDraftStore<TData> {
  drafts: Record<string, KeyedDraftEntry<TData>>;
  getDraft: (key: string) => TData | undefined;
  /** Merge `patch` into the key's entry; drops the entry if no longer meaningful. */
  setDraft: (key: string, patch: Partial<TData>) => void;
  /** Call on successful send/submit or explicit discard — never on failure. */
  clearDraft: (key: string) => void;
  hasDraft: (key: string) => boolean;
  /**
   * `setDraft` for a delayed write that belongs to `origin` (an editor flushing
   * on unmount, an upload settling). After a workspace switch it writes into the
   * origin workspace's persisted namespace and never touches the current
   * workspace's drafts. After logout or workspace delete cleared the origin
   * (stale generation) it is dropped.
   */
  setDraftIn: (origin: DraftOrigin, key: string, patch: Partial<TData>) => void;
  /**
   * True when this tab holds the key but shared storage no longer does: another tab logged out
   * or deleted the workspace and cleared storage, and this tab has not heard of it. A delayed
   * write that persisted its in-memory copy would bring the cleared draft back.
   */
  isClearedElsewhere: (key: string) => boolean;
  /** `clearDraft` for a delayed clear; see {@link KeyedDraftStore.setDraftIn}. */
  clearDraftIn: (origin: DraftOrigin, key: string) => void;
}

export const KEYED_DRAFT_TTL_MS = 30 * 24 * 60 * 60 * 1000;

export function createKeyedDraftStore<TData extends object>(
  config: KeyedDraftStoreConfig<TData>,
): UseBoundStore<StoreApi<KeyedDraftStore<TData>>> {
  const workspaceScoped = config.workspaceScoped ?? true;
  const ttlMs = config.ttlMs ?? KEYED_DRAFT_TTL_MS;
  const backing = config.storage ?? defaultStorage;
  const empty = () => structuredClone(config.emptyData);

  const prune = (
    drafts: Record<string, KeyedDraftEntry<TData>> | undefined,
  ): Record<string, KeyedDraftEntry<TData>> => {
    const cutoff = Date.now() - ttlMs;
    const out: Record<string, KeyedDraftEntry<TData>> = {};
    for (const [key, entry] of Object.entries(drafts ?? {})) {
      if (!entry || typeof entry.updatedAt !== "number" || entry.updatedAt < cutoff) continue;
      // Overlay on emptyData so entries from older builds regain later fields.
      const data = { ...empty(), ...entry.data };
      if (config.hasMeaningful(data)) out[key] = { data, updatedAt: entry.updatedAt };
    }
    return out;
  };

  type Drafts = Record<string, KeyedDraftEntry<TData>>;
  const withPatch = (drafts: Drafts, key: string, patch: Partial<TData>): Drafts => {
    const data = { ...(drafts[key]?.data ?? empty()), ...patch };
    if (!config.hasMeaningful(data)) return withoutKey(drafts, key);
    return { ...drafts, [key]: { data, updatedAt: Date.now() } };
  };
  const withoutKey = (drafts: Drafts, key: string): Drafts => {
    if (!(key in drafts)) return drafts;
    const { [key]: _dropped, ...rest } = drafts;
    return rest;
  };

  // Read-modify-write one other workspace's persisted namespace directly. The
  // in-memory map belongs to the current workspace and is left alone.
  const updateOtherWorkspace = (slug: string, update: (d: Drafts) => Drafts) => {
    const name = `${config.storageKey}:${slug}`;
    let persisted: { state?: { drafts?: Drafts }; version?: number } | null = null;
    try {
      const raw = backing.getItem(name);
      persisted = raw ? JSON.parse(raw) : null;
    } catch {
      persisted = null; // unreadable entry: start that namespace over
    }
    const drafts = update(prune(persisted?.state?.drafts));
    backing.setItem(
      name,
      JSON.stringify({ state: { drafts }, version: persisted?.version ?? 0 }),
    );
  };
  const inOtherWorkspace = (slug: string | null) =>
    workspaceScoped && slug !== getCurrentSlug();

  // Slug the in-memory map was loaded from. `setCurrentWorkspace` changes the
  // slug synchronously but rehydrates in a microtask. In that gap the map
  // belongs to the old workspace: reads treat it as empty (never leak it into
  // the new workspace) and writes rehydrate first (never persist it under the
  // new slug).
  let loadedSlug: string | null = null;
  const isStale = () => workspaceScoped && getCurrentSlug() !== loadedSlug;
  const syncToWorkspace = () => {
    if (isStale()) useStore.persist.rehydrate();
  };

  const useStore = create<KeyedDraftStore<TData>>()(
    persist(
      (set, get) => ({
        drafts: {},
        getDraft: (key) => (isStale() ? undefined : get().drafts[key]?.data),
        setDraft: (key, patch) => {
          syncToWorkspace();
          set((s) => {
            const drafts = withPatch(s.drafts, key, patch);
            return drafts === s.drafts ? s : { drafts };
          });
        },
        clearDraft: (key) => {
          syncToWorkspace();
          set((s) => {
            const drafts = withoutKey(s.drafts, key);
            return drafts === s.drafts ? s : { drafts };
          });
        },
        hasDraft: (key) => !isStale() && key in get().drafts,
        isClearedElsewhere: (key) => {
          const slug = getCurrentSlug();
          if (isStale() || !(key in get().drafts) || (workspaceScoped && !slug)) return false;
          try {
            const raw = backing.getItem(workspaceScoped ? `${config.storageKey}:${slug}` : config.storageKey);
            const persisted = raw ? (JSON.parse(raw) as { state?: { drafts?: Drafts } }) : null;
            return !persisted?.state?.drafts?.[key];
          } catch {
            return false; // unreadable entry: do not guess
          }
        },
        setDraftIn: ({ slug, generation }, key, patch) => {
          if (generation !== getDraftGeneration(slug)) return;
          if (!inOtherWorkspace(slug)) return get().setDraft(key, patch);
          if (slug) updateOtherWorkspace(slug, (d) => withPatch(d, key, patch));
        },
        clearDraftIn: ({ slug, generation }, key) => {
          if (generation !== getDraftGeneration(slug)) return;
          if (!inOtherWorkspace(slug)) return get().clearDraft(key);
          if (slug) updateOtherWorkspace(slug, (d) => withoutKey(d, key));
        },
      }),
      {
        name: config.storageKey,
        storage: createJSONStorage(() =>
          workspaceScoped ? createWorkspaceAwareStorage(backing) : backing,
        ),
        partialize: (s) => ({ drafts: s.drafts }) as KeyedDraftStore<TData>,
        // An empty namespace must mean an empty draft set — zustand's default
        // merge would keep the previous workspace's drafts.
        merge: (persisted, current) => {
          loadedSlug = getCurrentSlug();
          return {
            ...current,
            drafts: prune(
              (persisted as { drafts?: Record<string, KeyedDraftEntry<TData>> } | undefined)?.drafts,
            ),
          };
        },
      },
    ),
  );

  if (workspaceScoped) {
    registerForWorkspaceRehydration(() => useStore.persist.rehydrate());
  }

  registerDraftCleanup({
    storageKey: config.storageKey,
    workspaceScoped,
    resetInMemory: () => useStore.setState({ drafts: {} }),
  });

  return useStore;
}
