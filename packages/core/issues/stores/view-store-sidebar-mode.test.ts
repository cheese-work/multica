// @vitest-environment node
import { describe, expect, it } from "vitest";
import { createJSONStorage, persist } from "zustand/middleware";
import { createStore } from "zustand/vanilla";
import { mergeViewStatePersisted, viewStorePersistOptions, viewStoreSlice, type IssueViewState } from "./view-store";

// CHE-4358: the sidebar card / dialog mode is one string id per device, persisted in the view store.
const memory = () => {
  const data = new Map<string, string>();
  return { getItem: (k: string) => data.get(k) ?? null, setItem: (k: string, v: string) => void data.set(k, v), removeItem: (k: string) => void data.delete(k) };
};
const make = (storage: ReturnType<typeof memory>) =>
  createStore<IssueViewState>()(persist(viewStoreSlice, { ...viewStorePersistOptions("t"), storage: createJSONStorage(() => storage) }));

describe("sidebarMode", () => {
  it("defaults to hierarchy and survives reload as a string id", async () => {
    const storage = memory();
    const first = make(storage);
    expect(first.getState().sidebarMode).toBe("hierarchy");
    first.getState().setSidebarMode("swimlane2");

    const second = make(storage);
    await second.persist.rehydrate();
    expect(second.getState().sidebarMode).toBe("swimlane2");
  });

  it("keeps an id the store does not know (the registry resolves it); drops a non-string", () => {
    const current = createStore<IssueViewState>()((set) => viewStoreSlice(set)).getState();
    expect(mergeViewStatePersisted({ sidebarMode: "removed-mode" }, current).sidebarMode).toBe("removed-mode");
    expect(mergeViewStatePersisted({ sidebarMode: 7 }, current).sidebarMode).toBe("hierarchy");
    expect(mergeViewStatePersisted({}, current).sidebarMode).toBe("hierarchy");
  });
});
