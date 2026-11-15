"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import type { SettingsSaveStatus } from "./settings-layout";

interface UseAutoSaveOptions<T> {
  value: T;
  savedValue: T;
  onSave: (value: T) => Promise<void>;
  onSuccess?: (value: T) => void;
  /** Value equals what is saved and no request is running. */
  onIdle?: (value: T) => void;
  onError?: (error: unknown) => void;
  enabled?: boolean;
  delay?: number;
  isEqual: (left: T, right: T) => boolean;
}

interface AutoSaveResult<T> {
  status: SettingsSaveStatus;
  flush: () => void;
  saveNow: (value: T) => void;
}

/**
 * Debounces text-heavy settings while serializing requests. If a user edits
 * again during an in-flight save, only the latest queued value is persisted
 * next, so a slower response can never overwrite a newer request.
 */
export function useAutoSave<T>({
  value,
  savedValue,
  onSave,
  onSuccess,
  onIdle,
  onError,
  enabled = true,
  delay = 650,
  isEqual,
}: UseAutoSaveOptions<T>): AutoSaveResult<T> {
  const [status, setStatus] = useState<SettingsSaveStatus>("idle");
  const mountedRef = useRef(true);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const savingRef = useRef(false);
  const queuedRef = useRef<T | null>(null);
  const latestValueRef = useRef(value);
  const persistedRef = useRef(savedValue);
  const observedSavedRef = useRef(savedValue);
  const enabledRef = useRef(enabled);
  const onSaveRef = useRef(onSave);
  const onSuccessRef = useRef(onSuccess);
  const onIdleRef = useRef(onIdle);
  const onErrorRef = useRef(onError);
  const isEqualRef = useRef(isEqual);

  latestValueRef.current = value;
  enabledRef.current = enabled;
  onSaveRef.current = onSave;
  onSuccessRef.current = onSuccess;
  onIdleRef.current = onIdle;
  onErrorRef.current = onError;
  isEqualRef.current = isEqual;

  if (!isEqual(savedValue, observedSavedRef.current)) {
    observedSavedRef.current = savedValue;
    persistedRef.current = savedValue;
  }

  const runSave = useCallback(async (next: T) => {
    if (!enabledRef.current || isEqualRef.current(next, persistedRef.current)) {
      return;
    }
    if (savingRef.current) {
      queuedRef.current = next;
      return;
    }

    savingRef.current = true;
    let succeeded = false;
    if (mountedRef.current) setStatus("saving");
    try {
      await onSaveRef.current(next);
      persistedRef.current = next;
      succeeded = true;
    } catch (error) {
      if (mountedRef.current) setStatus("error");
      onErrorRef.current?.(error);
    } finally {
      savingRef.current = false;
      const queued = queuedRef.current;
      queuedRef.current = null;
      const latest = latestValueRef.current;
      if (queued && !isEqualRef.current(queued, persistedRef.current)) {
        void runSave(queued);
      } else if (
        succeeded &&
        !timerRef.current &&
        !isEqualRef.current(latest, persistedRef.current)
      ) {
        // An edit made during the request matched the old saved value, so no
        // debounce was armed for it; send it now that the request is done.
        void runSave(latest);
      } else if (succeeded && mountedRef.current) {
        setStatus("saved");
        onSuccessRef.current?.(next);
      }
    }
  }, []);

  const saveNow = useCallback(
    (next: T) => {
      if (timerRef.current) {
        clearTimeout(timerRef.current);
        timerRef.current = null;
      }
      void runSave(next);
    },
    [runSave],
  );

  const flush = useCallback(() => {
    saveNow(latestValueRef.current);
  }, [saveNow]);

  useEffect(() => {
    if (timerRef.current) clearTimeout(timerRef.current);

    if (!enabled || isEqual(value, persistedRef.current)) {
      timerRef.current = null;
      if (!enabled) setStatus("idle");
      else if (!savingRef.current) onIdleRef.current?.(value);
      return;
    }

    setStatus("saving");
    timerRef.current = setTimeout(() => {
      timerRef.current = null;
      void runSave(latestValueRef.current);
    }, delay);

    return () => {
      if (timerRef.current) {
        clearTimeout(timerRef.current);
        timerRef.current = null;
      }
    };
  }, [delay, enabled, isEqual, runSave, value]);

  useEffect(() => {
    // Restore the invariant on every mount. Under React StrictMode the initial
    // setup/cleanup/setup cycle would otherwise leave mountedRef pinned to false
    // for the component's whole life, stranding status at "saving".
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      if (timerRef.current) clearTimeout(timerRef.current);
    };
  }, []);

  return { status, flush, saveNow };
}
