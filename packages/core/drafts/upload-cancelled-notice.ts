import { useSyncExternalStore } from "react";
import { getCurrentSlug } from "../platform/workspace-storage";
import { registerDraftCleanup } from "./cleanup-registry";

/**
 * In-memory only (never persisted) record of draft keys whose upload was cancelled because the
 * user left the card. Persisting it would write the whole draft map back to shared storage on
 * unmount, which can restore drafts another tab already cleared on logout (CHE-3521). A reload
 * therefore forgets the notice, and logout clears it with the drafts.
 */
let cancelled = new Set<string>();
const listeners = new Set<() => void>();
const emit = () => listeners.forEach((listener) => listener());
const idOf = (key: string) => `${getCurrentSlug() ?? ""}\u0000${key}`;

registerDraftCleanup({
  storageKey: "multica_upload_cancelled_notice",
  workspaceScoped: false,
  resetInMemory: () => {
    cancelled = new Set();
    emit();
  },
});

export function setUploadCancelledNotice(key: string, on: boolean): void {
  const id = idOf(key);
  if (cancelled.has(id) === on) return;
  const next = new Set(cancelled);
  if (on) next.add(id);
  else next.delete(id);
  cancelled = next;
  emit();
}

export function useUploadCancelledNotice(key: string): boolean {
  return useSyncExternalStore(
    (listener) => {
      listeners.add(listener);
      return () => { listeners.delete(listener); };
    },
    () => cancelled.has(idOf(key)),
    () => false,
  );
}
