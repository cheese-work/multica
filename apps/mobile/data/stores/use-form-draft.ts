/**
 * Restores a saved form draft once the disk has loaded. Returns true after
 * that; a screen must not write its draft before then, or its empty initial
 * state would replace the saved one. The flag lives in the store so the
 * screen's unmount resets it and the next mount restores again.
 */
import { useEffect, useRef } from "react";
import { readFormDraft, useFormDraftsStore } from "./form-drafts-store";

export function useRestoredFormDraft<T>(
  key: string,
  apply: (draft: T) => void,
): boolean {
  const ready = useFormDraftsStore((s) => s.ready);
  const restored = useFormDraftsStore((s) => s.restored[key] === true);
  const applyRef = useRef(apply);
  useEffect(() => {
    applyRef.current = apply;
  });
  useEffect(() => {
    if (!ready || restored) return;
    const draft = readFormDraft<T>(key);
    if (draft) applyRef.current(draft);
    useFormDraftsStore.getState().markRestored(key, true);
  }, [ready, restored, key]);
  useEffect(
    () => () => useFormDraftsStore.getState().markRestored(key, false),
    [key],
  );
  return restored;
}
