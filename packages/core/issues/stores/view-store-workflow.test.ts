// @vitest-environment node
import { describe, expect, it } from "vitest";
import { createStore } from "zustand/vanilla";
import { mergeViewStatePersisted, viewStoreSlice, type IssueViewState } from "./view-store";

const newStore = () => createStore<IssueViewState>()((set) => viewStoreSlice(set));

describe("workflowFilters", () => {
  it("toggles a workflow on and off", () => {
    const store = newStore();
    store.getState().toggleWorkflowFilter("w-1");
    store.getState().toggleWorkflowFilter("w-2");
    expect(store.getState().workflowFilters).toEqual(["w-1", "w-2"]);
    store.getState().toggleWorkflowFilter("w-1");
    expect(store.getState().workflowFilters).toEqual(["w-2"]);
  });

  it("clears with its own dimension and with clearFilters", () => {
    const store = newStore();
    store.getState().toggleWorkflowFilter("w-1");
    store.getState().clearFilterDimension("workflow");
    expect(store.getState().workflowFilters).toEqual([]);

    store.getState().toggleWorkflowFilter("w-1");
    store.getState().clearFilters();
    expect(store.getState().workflowFilters).toEqual([]);
  });

  it("survives a persisted round trip and ignores junk", () => {
    const base = newStore().getState();
    expect(mergeViewStatePersisted({ workflowFilters: ["w-1"] }, base).workflowFilters).toEqual(["w-1"]);
    expect(mergeViewStatePersisted({ workflowFilters: "x" as never }, base).workflowFilters).toEqual([]);
  });
});
