import { useCallback, useRef } from "react";
import type { StoreApi, UseBoundStore } from "zustand";
import { getCurrentSlug } from "../platform/workspace-storage";
import { localDraftGeneration } from "./cleanup-registry";
import type { DraftOrigin } from "./cleanup-registry";
import type { KeyedDraftStore } from "./create-keyed-draft-store";

/**
 * Bind one key of a keyed draft store to a component. Returns
 * `[draft, setDraft, clearDraft]`; `draft` is `empty` until the key has an
 * entry, so the value is always renderable and survives remounts.
 *
 * `empty` must be a stable reference (module constant): it is the selector's
 * fallback, and a fresh object per render would re-render on every store touch.
 *
 * Writes are bound to the workspace the component last rendered in. A late
 * write (an editor flushing on unmount, an upload settling) after the route
 * moved to another workspace lands in the originating workspace's drafts, never
 * the current one's. A write that outlives logout or a workspace delete is
 * dropped, so it cannot bring back a draft that cleanup removed. That holds
 * across tabs too: logout in another tab invalidates this tab's pending writes.
 */
export function usePersistedDraft<TData extends object>(
  store: UseBoundStore<StoreApi<KeyedDraftStore<TData>>>,
  key: string,
  empty: TData,
): [TData, (patch: Partial<TData>) => void, () => void] {
  const draft = store((s) => s.getDraft(key) ?? empty);
  const slug = getCurrentSlug();
  // The session a write belongs to is taken when the component first renders in a workspace, and
  // refreshed only when this tab's own cleanup ran (a logout and re-login without an unmount). A
  // cleanup in another tab never refreshes it, so a pending edit typed before that logout stays stale.
  const origins = useRef(new Map<string | null, { local: number; origin: DraftOrigin }>());
  const local = localDraftGeneration(slug);
  if (origins.current.get(slug)?.local !== local) origins.current.set(slug, { local, origin: store.getState().originFor(slug) });
  const origin = origins.current.get(slug)!.origin;
  const setDraft = useCallback(
    (patch: Partial<TData>) => {
      store.getState().setDraftIn(origin, key, patch);
    },
    [store, key, origin],
  );
  const clearDraft = useCallback(() => {
    store.getState().clearDraftIn(origin, key);
  }, [store, key, origin]);
  return [draft, setDraft, clearDraft];
}
