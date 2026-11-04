import { useCallback } from "react";
import type { StoreApi, UseBoundStore } from "zustand";
import type { KeyedDraftStore } from "./create-keyed-draft-store";

/**
 * Bind one key of a keyed draft store to a component. Returns
 * `[draft, setDraft, clearDraft]`; `draft` is `empty` until the key has an
 * entry, so the value is always renderable and survives remounts.
 *
 * `empty` must be a stable reference (module constant): it is the selector's
 * fallback, and a fresh object per render would re-render on every store touch.
 */
export function usePersistedDraft<TData extends object>(
  store: UseBoundStore<StoreApi<KeyedDraftStore<TData>>>,
  key: string,
  empty: TData,
): [TData, (patch: Partial<TData>) => void, () => void] {
  const draft = store((s) => s.drafts[key]?.data ?? empty);
  const setDraft = useCallback(
    (patch: Partial<TData>) => store.getState().setDraft(key, patch),
    [store, key],
  );
  const clearDraft = useCallback(() => store.getState().clearDraft(key), [store, key]);
  return [draft, setDraft, clearDraft];
}
