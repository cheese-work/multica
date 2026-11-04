import { create, type StoreApi, type UseBoundStore } from "zustand";
import { createJSONStorage, persist } from "zustand/middleware";
import {
  createWorkspaceAwareStorage,
  registerForWorkspaceRehydration,
} from "../platform/workspace-storage";
import { defaultStorage } from "../platform/storage";
import type { StorageAdapter } from "../types/storage";
import { registerDraftCleanup } from "./cleanup-registry";

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

  const useStore = create<KeyedDraftStore<TData>>()(
    persist(
      (set, get) => ({
        drafts: {},
        getDraft: (key) => get().drafts[key]?.data,
        setDraft: (key, patch) =>
          set((s) => {
            const data = { ...(s.drafts[key]?.data ?? empty()), ...patch };
            if (!config.hasMeaningful(data)) {
              if (!(key in s.drafts)) return s;
              const { [key]: _dropped, ...rest } = s.drafts;
              return { drafts: rest };
            }
            return { drafts: { ...s.drafts, [key]: { data, updatedAt: Date.now() } } };
          }),
        clearDraft: (key) =>
          set((s) => {
            if (!(key in s.drafts)) return s;
            const { [key]: _dropped, ...rest } = s.drafts;
            return { drafts: rest };
          }),
        hasDraft: (key) => key in get().drafts,
      }),
      {
        name: config.storageKey,
        storage: createJSONStorage(() =>
          workspaceScoped ? createWorkspaceAwareStorage(backing) : backing,
        ),
        partialize: (s) => ({ drafts: s.drafts }) as KeyedDraftStore<TData>,
        // An empty namespace must mean an empty draft set — zustand's default
        // merge would keep the previous workspace's drafts.
        merge: (persisted, current) => ({
          ...current,
          drafts: prune(
            (persisted as { drafts?: Record<string, KeyedDraftEntry<TData>> } | undefined)?.drafts,
          ),
        }),
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
