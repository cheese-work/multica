// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { createKeyedDraftStore } from "./create-keyed-draft-store";
import { usePersistedDraft } from "./use-persisted-draft";
import { setCurrentWorkspace } from "../platform/workspace-storage";

interface Sample {
  text: string;
}
const EMPTY: Sample = { text: "" };

function makeStore(storageKey: string) {
  return createKeyedDraftStore<Sample>({
    storageKey,
    emptyData: EMPTY,
    hasMeaningful: (d) => d.text.length > 0,
    workspaceScoped: false,
    storage: { getItem: () => null, setItem: () => {}, removeItem: () => {}, keys: () => [] },
  });
}

describe("usePersistedDraft", () => {
  it("returns the stable empty value, then the draft after a remount", () => {
    const store = makeStore("h_remount");
    const first = renderHook(() => usePersistedDraft(store, "k", EMPTY));
    expect(first.result.current[0]).toBe(EMPTY);

    act(() => first.result.current[1]({ text: "unsent" }));
    expect(first.result.current[0]).toEqual({ text: "unsent" });

    first.unmount();
    const second = renderHook(() => usePersistedDraft(store, "k", EMPTY));
    expect(second.result.current[0]).toEqual({ text: "unsent" });
  });

  it("clear resets to empty; keys stay isolated; callbacks are stable", () => {
    const store = makeStore("h_clear");
    const a = renderHook(() => usePersistedDraft(store, "a", EMPTY));
    const b = renderHook(() => usePersistedDraft(store, "b", EMPTY));

    act(() => a.result.current[1]({ text: "A" }));
    expect(b.result.current[0]).toBe(EMPTY);

    const setBefore = a.result.current[1];
    act(() => a.result.current[2]());
    expect(a.result.current[0]).toBe(EMPTY);
    expect(a.result.current[1]).toBe(setBefore);
  });

  it("returns empty, not the old workspace's draft, in the gap before rehydration", async () => {
    const values = new Map<string, string>();
    const store = createKeyedDraftStore<Sample>({
      storageKey: "h_ws",
      emptyData: EMPTY,
      hasMeaningful: (d) => d.text.length > 0,
      storage: {
        getItem: (k) => values.get(k) ?? null,
        setItem: (k, v) => void values.set(k, v),
        removeItem: (k) => void values.delete(k),
        keys: () => [...values.keys()],
      },
    });
    setCurrentWorkspace("ws-a", "id-a");
    await new Promise((r) => queueMicrotask(() => r(null)));
    store.getState().setDraft("k", { text: "in A" });

    setCurrentWorkspace("ws-b", "id-b");
    const hook = renderHook(() => usePersistedDraft(store, "k", EMPTY));
    expect(hook.result.current[0]).toBe(EMPTY);

    await act(async () => {
      await new Promise((r) => queueMicrotask(() => r(null)));
    });
    expect(hook.result.current[0]).toBe(EMPTY);
  });
});
