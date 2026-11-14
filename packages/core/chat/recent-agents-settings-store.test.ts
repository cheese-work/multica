// @vitest-environment node
import { beforeEach, describe, expect, it, vi } from "vitest";

const disk = vi.hoisted(() => new Map<string, string>());
vi.mock("../platform/storage", () => ({
  defaultStorage: {
    getItem: (key: string) => disk.get(key) ?? null,
    setItem: (key: string, value: string) => disk.set(key, value),
    removeItem: (key: string) => disk.delete(key),
  },
}));

import { setCurrentWorkspace } from "../platform/workspace-storage";
import { useRecentAgentsSettingsStore } from "./recent-agents-settings-store";

beforeEach(() => {
  disk.clear();
  setCurrentWorkspace("acme", "ws-1");
  useRecentAgentsSettingsStore.setState({ showRecentAgents: true });
});

describe("recent agents settings store", () => {
  it("shows recent agents by default", () => {
    disk.clear();
    expect(useRecentAgentsSettingsStore.getState().showRecentAgents).toBe(true);
  });

  it("keeps the saved value across a reload", async () => {
    useRecentAgentsSettingsStore.getState().setShowRecentAgents(false);
    const saved = new Map(disk);
    expect(saved.size).toBeGreaterThan(0);

    // Reload: memory resets to the default, then the saved entries come back.
    useRecentAgentsSettingsStore.setState({ showRecentAgents: true });
    disk.clear();
    saved.forEach((value, key) => disk.set(key, value));
    await useRecentAgentsSettingsStore.persist.rehydrate();

    expect(useRecentAgentsSettingsStore.getState().showRecentAgents).toBe(false);
  });

  it("saves the value per workspace", async () => {
    useRecentAgentsSettingsStore.getState().setShowRecentAgents(false);

    setCurrentWorkspace("globex", "ws-2");
    await useRecentAgentsSettingsStore.persist.rehydrate();
    expect(useRecentAgentsSettingsStore.getState().showRecentAgents).toBe(true);

    setCurrentWorkspace("acme", "ws-1");
    await useRecentAgentsSettingsStore.persist.rehydrate();
    expect(useRecentAgentsSettingsStore.getState().showRecentAgents).toBe(false);
  });
});
