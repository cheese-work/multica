// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { createKeyedDraftStore } from "./create-keyed-draft-store";
import { usePersistedDraft } from "./use-persisted-draft";
import { setCurrentWorkspace } from "../platform/workspace-storage";
import { clearRegisteredWorkspaceDrafts, DRAFT_GENERATION_KEY, resetAllRegisteredDrafts } from "./cleanup-registry";

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
  describe("late write after a workspace switch", () => {
    async function setup() {
      const values = new Map<string, string>();
      const store = createKeyedDraftStore<Sample>({
        storageKey: "h_late",
        emptyData: EMPTY,
        hasMeaningful: (d) => d.text.length > 0,
        storage: {
          getItem: (k) => values.get(k) ?? null,
          setItem: (k, v) => void values.set(k, v),
          removeItem: (k) => void values.delete(k),
          keys: () => [...values.keys()],
        },
      });
      const settle = () =>
        act(async () => {
          await new Promise((r) => queueMicrotask(() => r(null)));
        });
      setCurrentWorkspace("ws-a", "id-a");
      await settle();
      const hookA = renderHook(() => usePersistedDraft(store, "create", EMPTY));
      return { store, hookA, settle, values };
    }

    it("lands an editor flush in the originating workspace, never the current one", async () => {
      const { store, hookA, settle } = await setup();
      const lateWriteFromA = hookA.result.current[1];

      // Route switches A -> B before A's component cleanup runs its late flush.
      setCurrentWorkspace("ws-b", "id-b");
      act(() => lateWriteFromA({ text: "A runbook" }));
      hookA.unmount();
      await settle();

      expect(store.getState().getDraft("create")).toBeUndefined(); // B stays empty

      setCurrentWorkspace("ws-a", "id-a");
      await settle();
      expect(store.getState().getDraft("create")).toEqual({ text: "A runbook" });
    });

    it("keeps an upload that settles after the switch in the originating workspace", async () => {
      const { store, hookA, settle } = await setup();
      act(() => hookA.result.current[1]({ text: "before" }));
      const uploadSettles = hookA.result.current[1];

      setCurrentWorkspace("ws-b", "id-b");
      await settle();
      store.getState().setDraft("create", { text: "B own draft" });
      act(() => uploadSettles({ text: "A with upload" }));
      await settle();

      expect(store.getState().getDraft("create")).toEqual({ text: "B own draft" });
      setCurrentWorkspace("ws-a", "id-a");
      await settle();
      expect(store.getState().getDraft("create")).toEqual({ text: "A with upload" });
    });

    it("a late clear removes A's draft, not B's draft under the same key", async () => {
      const { store, hookA, settle } = await setup();
      act(() => hookA.result.current[1]({ text: "A" }));
      const lateClear = hookA.result.current[2];

      setCurrentWorkspace("ws-b", "id-b");
      await settle();
      store.getState().setDraft("create", { text: "B own draft" });
      act(() => lateClear());
      await settle();

      expect(store.getState().getDraft("create")).toEqual({ text: "B own draft" });
      setCurrentWorkspace("ws-a", "id-a");
      await settle();
      expect(store.getState().getDraft("create")).toBeUndefined();
    });

    describe("after cleanup", () => {
      const adapterOf = (values: Map<string, string>) => ({
        getItem: (k: string) => values.get(k) ?? null,
        setItem: (k: string, v: string) => void values.set(k, v),
        removeItem: (k: string) => void values.delete(k),
        keys: () => [...values.keys()],
      });

      it("logout, then a late editor flush: the draft does not come back", async () => {
        const { store, hookA, settle, values } = await setup();
        act(() => hookA.result.current[1]({ text: "private" }));
        const lateFlush = hookA.result.current[1];

        resetAllRegisteredDrafts(); // logout: memory first, then persisted keys
        clearRegisteredWorkspaceDrafts(adapterOf(values), "ws-a");
        act(() => lateFlush({ text: "private, late" }));
        hookA.unmount();
        await settle();

        expect(store.getState().getDraft("create")).toBeUndefined();
        expect([...values.keys()].filter((k) => k.startsWith("h_late"))).toEqual([]);
      });

      it("workspace delete after a switch, then a late upload settle: stays deleted", async () => {
        const { store, hookA, settle, values } = await setup();
        act(() => hookA.result.current[1]({ text: "A" }));
        const lateUpload = hookA.result.current[1];

        setCurrentWorkspace("ws-b", "id-b");
        await settle();
        clearRegisteredWorkspaceDrafts(adapterOf(values), "ws-a"); // A deleted
        act(() => lateUpload({ text: "A with upload" }));
        await settle();

        setCurrentWorkspace("ws-a", "id-a");
        await settle();
        expect(store.getState().getDraft("create")).toBeUndefined();
      });

      // Another tab's logout is only visible through shared storage: this tab's counters never moved.
      const otherTabLogsOut = (values: Map<string, string>) => {
        const key = DRAFT_GENERATION_KEY;
        window.localStorage.setItem(key, String(Number(window.localStorage.getItem(key) ?? 0) + 1));
        values.clear();
      };

      it("another tab logged out: a pending flush from this tab is dropped, even after a re-render", async () => {
        const { store, hookA, settle, values } = await setup();
        act(() => hookA.result.current[1]({ text: "private" }));
        otherTabLogsOut(values);
        hookA.rerender(); // a render before the unmount must not adopt the new session
        act(() => hookA.result.current[1]({ text: "private, late" }));
        hookA.unmount();
        await settle();

        expect([...values.keys()].filter((k) => k.startsWith("h_late"))).toEqual([]);
        expect(store.getState().getDraft("create")?.text).not.toBe("private, late");
      });

      it("another tab logged out: a late clear is dropped too", async () => {
        const { store, hookA, values } = await setup();
        act(() => hookA.result.current[1]({ text: "private" }));
        otherTabLogsOut(values);
        act(() => hookA.result.current[2]());
        expect(store.getState().getDraft("create")).toEqual({ text: "private" });
      });

      it("this tab's own logout and re-login keeps a mounted hook writing", async () => {
        const { store, hookA, settle, values } = await setup();
        resetAllRegisteredDrafts();
        clearRegisteredWorkspaceDrafts(adapterOf(values), "ws-a");
        hookA.rerender();
        act(() => hookA.result.current[1]({ text: "new session" }));
        await settle();
        expect(store.getState().getDraft("create")).toEqual({ text: "new session" });
      });

      it("a late clear after cleanup is a no-op and a fresh render writes again", async () => {
        const { store, hookA, settle, values } = await setup();
        resetAllRegisteredDrafts();
        clearRegisteredWorkspaceDrafts(adapterOf(values), "ws-a");
        act(() => hookA.result.current[2]());
        hookA.unmount();

        const next = renderHook(() => usePersistedDraft(store, "create", EMPTY));
        act(() => next.result.current[1]({ text: "new session" }));
        await settle();
        expect(store.getState().getDraft("create")).toEqual({ text: "new session" });
      });
    });
  });
});
