import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { createKeyedDraftStore } from "@multica/core/drafts";
import { useSettingsDraft } from "./use-settings-draft";
import { useAutoSave } from "./use-auto-save";

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((r) => (resolve = r));
  return { promise, resolve };
}

const store = createKeyedDraftStore<Record<string, string | undefined>>({
  storageKey: "h_settings_draft",
  emptyData: {},
  hasMeaningful: (d) => Object.values(d).some((v) => v !== undefined),
  workspaceScoped: false,
  storage: { getItem: () => null, setItem: () => {}, removeItem: () => {}, keys: () => [] },
});

// A settings field wired the way the settings tabs wire it.
function useField(onSave: (next: string) => Promise<void>) {
  const [saved, setSaved] = useState("A");
  const savedFields = { text: saved };
  const [fields, setFields, confirm] = useSettingsDraft(store, "k", savedFields);
  useAutoSave({
    value: fields.text,
    savedValue: saved,
    onSave: async (next) => {
      await onSave(next);
      setSaved(next);
    },
    onSuccess: (sent) => confirm({ text: sent }),
    isEqual: (l, r) => l === r,
  });
  return { text: fields.text, saved, setText: (text: string) => setFields({ text }) };
}

describe("useSettingsDraft with auto-save", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    store.setState({ drafts: {} });
  });
  afterEach(() => vi.useRealTimers());

  it("keeps a reversal made while a save is in flight, and saves it next", async () => {
    const first = deferred();
    const onSave = vi.fn().mockReturnValueOnce(first.promise).mockResolvedValue(undefined);
    const { result } = renderHook(() => useField(onSave));

    act(() => result.current.setText("B"));
    await act(async () => void vi.advanceTimersByTime(700));
    expect(onSave).toHaveBeenCalledWith("B");

    // Back to the saved value before B returns.
    act(() => result.current.setText("A"));
    await act(async () => first.resolve());
    await act(async () => void vi.advanceTimersByTime(700));

    expect(onSave).toHaveBeenLastCalledWith("A");
    expect(result.current.text).toBe("A");
    expect(result.current.saved).toBe("A");
    // Both sends landed, so nothing is left to restore.
    expect(store.getState().getDraft("k")).toBeUndefined();
  });

  it("keeps the draft when the save fails", async () => {
    const onSave = vi.fn().mockRejectedValue(new Error("offline"));
    const { result } = renderHook(() => useField(onSave));
    act(() => result.current.setText("B"));
    await act(async () => void vi.advanceTimersByTime(700));
    expect(result.current.text).toBe("B");
    expect(store.getState().getDraft("k")).toEqual({ text: "B" });
  });
});
