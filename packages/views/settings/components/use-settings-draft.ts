import { useCallback, useMemo } from "react";
import type { StoreApi, UseBoundStore } from "zustand";
import { usePersistedDraft } from "@multica/core/drafts";
import type { KeyedDraftStore } from "@multica/core/drafts";
import type { SettingsFieldsDraft } from "@multica/core/settings/draft-stores";

const NO_DRAFT: SettingsFieldsDraft = {};

/**
 * Text fields that follow `saved` until edited. An edit lands in the persisted
 * draft for `key`, so it outlives a remount, route change or reload. A field
 * leaves the draft only through `confirm(sent)`, called once `sent` was saved:
 * it drops the fields still equal to what was sent, so an edit made while that
 * save was in flight stays. A failed save never calls it and keeps the draft.
 */
export function useSettingsDraft<F extends string>(
  store: UseBoundStore<StoreApi<KeyedDraftStore<SettingsFieldsDraft>>>,
  key: string,
  saved: Record<F, string>,
): [
  Record<F, string>,
  (patch: Partial<Record<F, string | undefined>>) => void,
  (sent: Partial<Record<F, string>>) => void,
] {
  const [draft, setDraft] = usePersistedDraft(store, key, NO_DRAFT);

  const values = useMemo(() => {
    const out = { ...saved };
    for (const field of Object.keys(saved) as F[]) out[field] = draft[field] ?? saved[field];
    return out;
  }, [draft, saved]);

  const confirm = useCallback(
    (sent: Partial<Record<F, string>>) => {
      const current: SettingsFieldsDraft = store.getState().getDraft(key) ?? NO_DRAFT;
      const settled = Object.keys(sent).filter(
        (field) => current[field] !== undefined && current[field] === sent[field as F],
      );
      if (settled.length) setDraft(Object.fromEntries(settled.map((f) => [f, undefined])));
    },
    [store, key, setDraft],
  );

  return [values, setDraft as (patch: Partial<Record<F, string | undefined>>) => void, confirm];
}
