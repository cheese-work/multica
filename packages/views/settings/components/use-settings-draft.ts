import { useEffect, useMemo } from "react";
import type { StoreApi, UseBoundStore } from "zustand";
import { usePersistedDraft } from "@multica/core/drafts";
import type { KeyedDraftStore } from "@multica/core/drafts";
import type { SettingsFieldsDraft } from "@multica/core/settings/draft-stores";

const NO_DRAFT: SettingsFieldsDraft = {};

/**
 * Text fields that follow `saved` until edited. An edit lands in the persisted
 * draft for `key`, so it outlives a remount, route change or reload; once
 * `saved` catches up (the save succeeded) the field drops out of the draft.
 * A failed save leaves the draft in place.
 */
export function useSettingsDraft<F extends string>(
  store: UseBoundStore<StoreApi<KeyedDraftStore<SettingsFieldsDraft>>>,
  key: string,
  saved: Record<F, string>,
): [Record<F, string>, (patch: Partial<Record<F, string | undefined>>) => void] {
  const [draft, setDraft] = usePersistedDraft(store, key, NO_DRAFT);

  useEffect(() => {
    const confirmed = Object.keys(draft).filter(
      (field) => draft[field] !== undefined && draft[field] === saved[field as F],
    );
    if (confirmed.length) setDraft(Object.fromEntries(confirmed.map((f) => [f, undefined])));
  }, [draft, saved, setDraft]);

  const values = useMemo(() => {
    const out = { ...saved };
    for (const field of Object.keys(saved) as F[]) out[field] = draft[field] ?? saved[field];
    return out;
  }, [draft, saved]);
  return [values, setDraft as (patch: Partial<Record<F, string | undefined>>) => void];
}
