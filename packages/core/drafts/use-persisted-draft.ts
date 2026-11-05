import { useCallback } from "react";
import type { StoreApi, UseBoundStore } from "zustand";
import { getCurrentSlug } from "../platform/workspace-storage";
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
 * the current one's.
 */
export function usePersistedDraft<TData extends object>(
  store: UseBoundStore<StoreApi<KeyedDraftStore<TData>>>,
  key: string,
  empty: TData,
): [TData, (patch: Partial<TData>) => void, () => void] {
  const draft = store((s) => s.getDraft(key) ?? empty);
  const slug = getCurrentSlug();
  const setDraft = useCallback(
    (patch: Partial<TData>) => {
      store.getState().setDraftIn(slug, key, patch);
    },
    [store, key, slug],
  );
  const clearDraft = useCallback(() => {
    store.getState().clearDraftIn(slug, key);
  }, [store, key, slug]);
  return [draft, setDraft, clearDraft];
}
