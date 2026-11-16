// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { createKeyedDraftStore } from "./create-keyed-draft-store";
import { usePersistedDraft } from "./use-persisted-draft";
import { setCurrentWorkspace } from "../platform/workspace-storage";
import { clearRegisteredWorkspaceDrafts, resetAllRegisteredDrafts } from "./cleanup-registry";

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

      // Another tab is a second copy of the module sharing this page's localStorage. Its logout is the real
      // one: end the session, then clear the stored drafts. `duringGap` runs between those two steps.
      const loadTabB = async (storageKey: string) => {
        vi.resetModules();
        const tabB = await import("./cleanup-registry");
        tabB.registerDraftCleanup({ storageKey, workspaceScoped: true, resetInMemory: () => {} });
        return tabB;
      };
      const logoutIn = (tabB: Awaited<ReturnType<typeof loadTabB>>, values: Map<string, string>, duringGap?: () => void) => {
        tabB.resetAllRegisteredDrafts();
        let gapDone = false;
        tabB.clearRegisteredWorkspaceDrafts(
          {
            ...adapterOf(values),
            removeItem: (k) => {
              if (!gapDone) {
                gapDone = true;
                duringGap?.();
              }
              values.delete(k);
            },
          },
          "ws-a",
        );
      };
      const otherTabLogsOut = async (values: Map<string, string>, duringGap?: () => void) =>
        logoutIn(await loadTabB("h_late"), values, duringGap);

      it("another tab logout gap: a tab that mounts and writes in the gap cannot bring the old note back", async () => {
        const { store, hookA, settle, values } = await setup();
        act(() => hookA.result.current[1]({ text: "private" }));
        await otherTabLogsOut(values, () => {
          const fresh = renderHook(() => usePersistedDraft(store, "other", EMPTY));
          act(() => fresh.result.current[1]({ text: "gap write" }));
        });
        store.getState().setDraft("third", { text: "later edit" }); // after the clear, a different key
        await settle();
        const persisted = JSON.stringify([...values.entries()].filter(([k]) => k.startsWith("h_late")));
        expect(persisted).not.toContain("private");
      });

      it("another tab deletes the workspace: a write in the gap cannot bring the old note back", async () => {
        const { store, hookA, settle, values } = await setup();
        act(() => hookA.result.current[1]({ text: "private" }));
        vi.resetModules();
        const tabB = await import("./cleanup-registry");
        tabB.registerDraftCleanup({ storageKey: "h_late", workspaceScoped: true, resetInMemory: () => {} });
        let gapDone = false;
        tabB.clearRegisteredWorkspaceDrafts( // workspace delete only: no logout
          {
            ...adapterOf(values),
            removeItem: (k) => {
              if (!gapDone) {
                gapDone = true;
                const fresh = renderHook(() => usePersistedDraft(store, "other", EMPTY));
                act(() => fresh.result.current[1]({ text: "gap write" }));
              }
              values.delete(k);
            },
          },
          "ws-a",
        );
        store.getState().setDraft("third", { text: "later edit" });
        await settle();
        const persisted = JSON.stringify([...values.entries()].filter(([k]) => k.startsWith("h_late")));
        expect(persisted).not.toContain("private");
      });

      // A write decided under the old session lands after the other tab's whole logout. The next user
      // signs in on the same profile and may already have saved a draft under the same key.
      const lateLandingSetup = async () => {
        const values = new Map<string, string>();
        const tabB = await loadTabB("h_land");
        let armed = false;
        let newUserWrites: (() => void) | undefined;
        const make = (storage: ReturnType<typeof adapterOf>) =>
          createKeyedDraftStore<Sample>({
            storageKey: "h_land",
            emptyData: EMPTY,
            hasMeaningful: (d) => d.text.length > 0,
            storage,
          });
        const store = make({
          ...adapterOf(values),
          setItem: (k, v) => {
            if (armed) {
              armed = false;
              logoutIn(tabB, values);
              newUserWrites?.();
            }
            values.set(k, v);
          },
        });
        setCurrentWorkspace("ws-a", "id-a");
        await act(async () => { await new Promise((r) => queueMicrotask(() => r(null))); });
        const lateWrite = (onLogout?: () => void) => {
          newUserWrites = onLogout;
          armed = true;
          store.getState().setDraft("create", { text: "user one note" });
        };
        const rawStorage = () => JSON.stringify([...values.entries()].filter(([k]) => k.startsWith("h_land")));
        const storageEvent = () => window.dispatchEvent(new Event("storage")); // another tab wrote a key
        return { make, lateWrite, rawStorage, storageEvent, values };
      };

      it("a write that lands after another tab's logout cleared storage is not read, and is swept from storage", async () => {
        const { make, lateWrite, rawStorage, storageEvent, values } = await lateLandingSetup();
        lateWrite();
        // the next user signs in on the same profile: a page load reads storage
        expect(make(adapterOf(values)).getState().getDraft("create")).toBeUndefined();
        storageEvent();
        expect(rawStorage()).not.toContain("user one note");
      });

      it("a write that lands after another tab's logout does not replace the next user's draft", async () => {
        const { make, lateWrite, rawStorage, storageEvent, values } = await lateLandingSetup();
        lateWrite(() => {
          const next = make(adapterOf(values));
          next.getState().setDraft("create", { text: "user two note" });
        });
        storageEvent();
        expect(make(adapterOf(values)).getState().getDraft("create")).toEqual({ text: "user two note" });
        expect(rawStorage()).not.toContain("user one note");
      });

      it("another tab logged out: a pending flush from this tab is dropped, even after a re-render", async () => {
        const { store, hookA, settle, values } = await setup();
        act(() => hookA.result.current[1]({ text: "private" }));
        await otherTabLogsOut(values);
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
        await otherTabLogsOut(values);
        act(() => hookA.result.current[2]());
        // The map is the old session's, so reads hide it; the dropped clear leaves it untouched.
        expect(store.getState().drafts.create?.data).toEqual({ text: "private" });
      });

      it("another tab logged out: a card mounted afterwards does not write the old drafts back", async () => {
        const { store, hookA, settle, values } = await setup();
        act(() => hookA.result.current[1]({ text: "private" }));
        await otherTabLogsOut(values);
        // This tab has not heard of the logout. A fresh mount must not adopt the old session's map.
        const fresh = renderHook(() => usePersistedDraft(store, "other", EMPTY));
        expect(store.getState().getDraft("create")).toBeUndefined();
        act(() => fresh.result.current[1]({ text: "new" }));
        await settle();
        const persisted = [...values.entries()].filter(([k]) => k.startsWith("h_late"));
        expect(JSON.stringify(persisted)).not.toContain("private");
        expect(JSON.stringify(persisted)).toContain("new");
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
