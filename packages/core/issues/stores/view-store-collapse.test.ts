// @vitest-environment node
import { describe, expect, it } from "vitest";
import { createJSONStorage, persist } from "zustand/middleware";
import { createStore } from "zustand/vanilla";
import { mergeViewStatePersisted, viewStorePersistOptions, viewStoreSlice, type IssueViewState } from "./view-store";

// CHE-4355: Gantt v2 row collapse is a set of issue ids per surface, persisted in the view store.
// Hierarchy and Swimlane 2.0 use the same key shape later, so one collapse state can serve every mode.
const memory = () => {
  const data = new Map<string, string>();
  return { getItem: (k: string) => data.get(k) ?? null, setItem: (k: string, v: string) => void data.set(k, v), removeItem: (k: string) => void data.delete(k) };
};
const make = (storage: ReturnType<typeof memory>) =>
  createStore<IssueViewState>()(
    persist(viewStoreSlice, { ...viewStorePersistOptions("t"), storage: createJSONStorage(() => storage) }),
  );

describe("collapsedIssues per surface", () => {
  it("collapse survives reload", async () => {
    const storage = memory();
    const first = make(storage);
    first.getState().toggleIssueCollapsed("gantt", "a");
    first.getState().toggleIssueCollapsed("gantt", "b");
    first.getState().toggleIssueCollapsed("gantt", "a"); // expand again

    const second = make(storage);
    await second.persist.rehydrate();
    expect(second.getState().collapsedIssues.gantt).toEqual(["b"]);
  });

  it("surfaces do not share a collapse set", () => {
    const store = make(memory());
    store.getState().toggleIssueCollapsed("gantt", "a");
    expect(store.getState().collapsedIssues).toEqual({ gantt: ["a"], swimlane2: [], hierarchy: [] });
  });

  it("a snapshot without or with a malformed collapse set falls back to empty", () => {
    const current = createStore<IssueViewState>()((set) => viewStoreSlice(set)).getState();
    expect(mergeViewStatePersisted({}, current).collapsedIssues.gantt).toEqual([]);
    const bad = mergeViewStatePersisted({ collapsedIssues: { gantt: ["a", 1, null], hierarchy: "x" } }, current);
    expect(bad.collapsedIssues).toEqual({ gantt: ["a"], swimlane2: [], hierarchy: [] });
  });
});
