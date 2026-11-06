import { useCallback, useState, useSyncExternalStore } from "react";
import { getCurrentSlug } from "../platform/workspace-storage";
import { registerDraftCleanup } from "./cleanup-registry";

/**
 * In-memory only (never persisted) record of draft keys whose upload was cancelled because the
 * user left the card. Persisting it would write the whole draft map back to shared storage on
 * unmount, which can restore drafts another tab already cleared on logout (CHE-3521). A reload
 * therefore forgets the notice, and logout clears it with the drafts.
 *
 * The notice belongs to the workspace the card mounted in. On a desktop workspace switch the
 * incoming route sets the new workspace before the old card's cleanup runs, so the slug is
 * captured at mount and never read live.
 */
let cancelled = new Set<string>();
const listeners = new Set<() => void>();
const emit = () => listeners.forEach((listener) => listener());
const idOf = (slug: string | null, key: string) => `${slug ?? ""}\u0000${key}`;

registerDraftCleanup({
  storageKey: "multica_upload_cancelled_notice",
  workspaceScoped: false,
  resetInMemory: () => {
    cancelled = new Set();
    emit();
  },
});

function setNotice(slug: string | null, key: string, on: boolean): void {
  const id = idOf(slug, key);
  if (cancelled.has(id) === on) return;
  const next = new Set(cancelled);
  if (on) next.add(id);
  else next.delete(id);
  cancelled = next;
  emit();
}

/** `[cancelled, set]` for one draft key, bound to the workspace the caller mounted in. */
export function useUploadCancelledNotice(key: string): [boolean, (on: boolean) => void] {
  const [slug] = useState(getCurrentSlug);
  const on = useSyncExternalStore(
    (listener) => {
      listeners.add(listener);
      return () => { listeners.delete(listener); };
    },
    () => cancelled.has(idOf(slug, key)),
    () => false,
  );
  const set = useCallback((next: boolean) => setNotice(slug, key, next), [slug, key]);
  return [on, set];
}
