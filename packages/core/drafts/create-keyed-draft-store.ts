import { create, type StoreApi, type UseBoundStore } from "zustand";
import { createJSONStorage, persist } from "zustand/middleware";
import {
  createWorkspaceAwareStorage,
  getCurrentSlug,
  registerForWorkspaceRehydration,
} from "../platform/workspace-storage";
import { defaultStorage } from "../platform/storage";
import type { StorageAdapter } from "../types/storage";
import {
  currentDraftOrigin,
  draftSessionToken,
  NO_DRAFT_SESSION,
  registerDraftCleanup,
  sessionKeySuffix,
  sweepDeadDrafts,
  type DraftOrigin,
} from "./cleanup-registry";

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
   * Origin for a delayed write, captured when it is scheduled: the workspace and
   * the session token then. Pass it to {@link KeyedDraftStore.setDraftIn} later.
   */
  originFor: (slug: string | null) => DraftOrigin;
  /**
   * `setDraft` for a delayed write that belongs to `origin` (an editor flushing
   * on unmount, an upload settling). After a workspace switch it writes into the
   * origin workspace's persisted namespace and never touches the current
   * workspace's drafts. After logout or workspace delete ended the origin's
   * session it is dropped. The storage key comes from `origin`, never from the
   * current session.
   */
  setDraftIn: (origin: DraftOrigin, key: string, patch: Partial<TData>) => void;
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
  const updateOtherWorkspace = (slug: string, token: string, update: (d: Drafts) => Drafts) => {
    const name = `${config.storageKey}:${slug}${sessionKeySuffix(token)}`;
    let persisted: { state?: { drafts?: Drafts }; version?: number } | null = null;
    try {
      const raw = backing.getItem(name);
      persisted = raw ? JSON.parse(raw) : null;
    } catch {
      persisted = null; // unreadable entry: start that namespace over
    }
    const drafts = update(prune(persisted?.state?.drafts));
    backing.setItem(name, JSON.stringify({ state: { drafts }, version: persisted?.version ?? 0 }));
  };
  const inOtherWorkspace = (slug: string | null) =>
    workspaceScoped && slug !== getCurrentSlug();

  // Slug the in-memory map was loaded from. `setCurrentWorkspace` changes the
  // slug synchronously but rehydrates in a microtask. In that gap the map
  // belongs to the old workspace: reads treat it as empty (never leak it into
  // the new workspace) and writes rehydrate first (never persist it under the
  // new slug).
  //
  // The same holds when another tab logged out or deleted this workspace: the
  // session token moved since the map was loaded, so the map is the old
  // session's. Reads treat it as empty and the next write reloads first.
  //
  // Storage keys carry the session token (`<name>#<token>`). A load reads only
  // the current token's key. Every write goes through `applyIn`, which reads the
  // token once, checks it and writes under that same token's key: the key is
  // never recomputed from the live session. A write that races a logout in
  // another process lands under a dead key, which `sweepDeadDrafts` removes.
  let loadedSlug: string | null = null;
  let loadedToken = NO_DRAFT_SESSION;
  const tokenNow = () => draftSessionToken(workspaceScoped ? loadedSlug : null);
  const isStale = () =>
    (workspaceScoped && getCurrentSlug() !== loadedSlug) || tokenNow() !== loadedToken;
  // Token the last load read its payload under. `merge` compares against this
  // value, never a fresh read: a logout landing between the read and the merge
  // must not let the old payload adopt the new session's token.
  let readToken = NO_DRAFT_SESSION;
  const sessioned: StorageAdapter = {
    getItem: (name) => {
      readToken = draftSessionToken(workspaceScoped ? getCurrentSlug() : null);
      return backing.getItem(name + sessionKeySuffix(readToken));
    },
    // `loadedToken` is the token the in-memory map was loaded under, so the map
    // is always written back to its own key. No live token read here.
    setItem: (name, value) => {
      if (!workspaceScoped || getCurrentSlug() === loadedSlug) {
        backing.setItem(name + sessionKeySuffix(loadedToken), value);
      }
    },
    removeItem: (name) => backing.removeItem(name + sessionKeySuffix(loadedToken)),
  };
  const tokenSlug = (slug: string | null) => (workspaceScoped ? slug : null);

  // The only write path: one synchronous step that checks the session and
  // writes. It reads the token once (`token`) and uses it for the check, the
  // reload decision and the storage key. `expected` is the token a delayed write
  // was scheduled under; null for a write that belongs to the live session.
  const applyIn = (slug: string | null, expected: string | null, update: (d: Drafts) => Drafts) => {
    const token = draftSessionToken(tokenSlug(slug));
    if (expected !== null && expected !== token) return;
    if (inOtherWorkspace(slug)) {
      if (slug) updateOtherWorkspace(slug, token, update);
      return;
    }
    if ((workspaceScoped && getCurrentSlug() !== loadedSlug) || token !== loadedToken) {
      useStore.persist.rehydrate();
      // The reload read its own token: a delayed write must still match it.
      if (expected !== null && expected !== loadedToken) return;
    }
    useStore.setState((s) => {
      const drafts = update(s.drafts);
      return drafts === s.drafts ? s : { drafts };
    });
  };
  const liveSlug = () => (workspaceScoped ? getCurrentSlug() : null);

  const useStore = create<KeyedDraftStore<TData>>()(
    persist(
      (_set, get) => ({
        drafts: {},
        getDraft: (key) => (isStale() ? undefined : get().drafts[key]?.data),
        setDraft: (key, patch) => applyIn(liveSlug(), null, (d) => withPatch(d, key, patch)),
        clearDraft: (key) => applyIn(liveSlug(), null, (d) => withoutKey(d, key)),
        hasDraft: (key) => !isStale() && key in get().drafts,
        originFor: (slug) => currentDraftOrigin(tokenSlug(slug)),
        setDraftIn: ({ slug, session }, key, patch) =>
          applyIn(slug, session, (d) => withPatch(d, key, patch)),
        clearDraftIn: ({ slug, session }, key) =>
          applyIn(slug, session, (d) => withoutKey(d, key)),
      }),
      {
        name: config.storageKey,
        storage: createJSONStorage(() =>
          workspaceScoped ? createWorkspaceAwareStorage(sessioned) : sessioned,
        ),
        // An empty namespace must mean an empty draft set — zustand's default
        // merge would keep the previous workspace's drafts.
        merge: (persisted, current) => {
          loadedSlug = getCurrentSlug();
          const moved = tokenNow() !== readToken; // session ended after the read: payload is the old session's
          loadedToken = readToken;
          const drafts = moved ? {} : prune((persisted as { drafts?: Drafts } | undefined)?.drafts);
          return { ...current, drafts };
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
    sessioned: backing,
    resetInMemory: () => {
      loadedToken = tokenNow(); // this tab's own logout: memory is empty, so it is current
      useStore.setState({ drafts: {} });
    },
  });
  sweepDeadDrafts();

  return useStore;
}
