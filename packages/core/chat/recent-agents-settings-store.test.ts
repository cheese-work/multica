// @vitest-environment jsdom
import { beforeAll, beforeEach, describe, expect, it } from "vitest";
import { setCurrentWorkspace } from "../platform/workspace-storage";
import { useRecentAgentsSettingsStore } from "./recent-agents-settings-store";

// Node 25 ships a partial `localStorage` shim under jsdom; replace it with a
// real in-memory Storage so persist can round-trip values.
beforeAll(() => {
  if (typeof globalThis.localStorage?.clear !== "function") {
    const values = new Map<string, string>();
    const storage: Storage = {
      get length() { return values.size; },
      clear: () => values.clear(),
      getItem: (k) => values.get(k) ?? null,
      key: (i) => Array.from(values.keys())[i] ?? null,
      removeItem: (k) => { values.delete(k); },
      setItem: (k, v) => { values.set(k, v); },
    };
    Object.defineProperty(globalThis, "localStorage", { configurable: true, value: storage });
    Object.defineProperty(window, "localStorage", { configurable: true, value: storage });
  }
});

describe("recent agents settings store", () => {
  beforeEach(() => {
    localStorage.clear();
    setCurrentWorkspace("acme", "ws-1");
    useRecentAgentsSettingsStore.setState({ showRecentAgents: true });
  });

  it("defaults to on", () => {
    expect(useRecentAgentsSettingsStore.getState().showRecentAgents).toBe(true);
  });

  it("persists the value per workspace", async () => {
    useRecentAgentsSettingsStore.getState().setShowRecentAgents(false);

    setCurrentWorkspace("other", "ws-2");
    await useRecentAgentsSettingsStore.persist.rehydrate();
    expect(useRecentAgentsSettingsStore.getState().showRecentAgents).toBe(true);

    setCurrentWorkspace("acme", "ws-1");
    await useRecentAgentsSettingsStore.persist.rehydrate();
    expect(useRecentAgentsSettingsStore.getState().showRecentAgents).toBe(false);
  });
});
