// @vitest-environment node
import { describe, expect, it } from "vitest";
import { createStore } from "zustand/vanilla";
import { viewStorePersistOptions, viewStoreSlice, type IssueViewState } from "./view-store";

// "Agent running" tracks second-to-second state. A stored toggle would bring a
// user back to an unexplained empty list, so it stays ephemeral (fork rule 1).
describe("agentRunningFilter is ephemeral", () => {
  it("is not written to persisted state", () => {
    const state = createStore<IssueViewState>()((set) => viewStoreSlice(set)).getState();
    const persisted = viewStorePersistOptions("t").partialize({ ...state, agentRunningFilter: true });
    expect(persisted).not.toHaveProperty("agentRunningFilter");
  });
});
