// @vitest-environment node
import { beforeEach, describe, expect, it } from "vitest";
import { createKeyedDraftStore } from "./create-keyed-draft-store";
import {
  resetAllRegisteredDrafts,
  clearRegisteredWorkspaceDrafts,
  registeredDraftKeys,
  __clearDraftCleanupRegistryForTest,
} from "./cleanup-registry";
import { setCurrentWorkspace } from "../platform/workspace-storage";
import type { StorageAdapter } from "../types/storage";

interface Sample {
  text: string;
  tags: string[];
}

const EMPTY: Sample = { text: "", tags: [] };
const DAY_MS = 24 * 60 * 60 * 1000;
const flush = () => new Promise((resolve) => queueMicrotask(() => resolve(null)));

function memoryStorage(seed: Record<string, string> = {}): StorageAdapter & {
  values: Map<string, string>;
} {
  const values = new Map(Object.entries(seed));
  return {
    values,
    getItem: (k) => values.get(k) ?? null,
    setItem: (k, v) => void values.set(k, v),
    removeItem: (k) => void values.delete(k),
    keys: () => [...values.keys()],
  };
}

function build(
  storageKey: string,
  storage = memoryStorage(),
  extra: { workspaceScoped?: boolean } = {},
) {
  const useStore = createKeyedDraftStore<Sample>({
    storageKey,
    emptyData: EMPTY,
    hasMeaningful: (d) => d.text.trim().length > 0 || d.tags.length > 0,
    workspaceScoped: false,
    storage,
    ...extra,
  });
  return { useStore, storage };
}

beforeEach(() => {
  __clearDraftCleanupRegistryForTest();
  setCurrentWorkspace(null, null);
});

describe("createKeyedDraftStore", () => {
  it("isolates entries by key and merges patches per key", () => {
    const { useStore } = build("k_isolate");
    const s = () => useStore.getState();

    s().setDraft("a", { text: "one" });
    s().setDraft("b", { tags: ["x"] });
    s().setDraft("a", { tags: ["y"] });

    expect(s().getDraft("a")).toEqual({ text: "one", tags: ["y"] });
    expect(s().getDraft("b")).toEqual({ text: "", tags: ["x"] });
    expect(s().getDraft("c")).toBeUndefined();
    expect(s().hasDraft("a")).toBe(true);
    expect(s().hasDraft("c")).toBe(false);
  });

  it("clearDraft drops only that key", () => {
    const { useStore } = build("k_clear");
    const s = () => useStore.getState();
    s().setDraft("a", { text: "one" });
    s().setDraft("b", { text: "two" });

    s().clearDraft("a");

    expect(s().hasDraft("a")).toBe(false);
    expect(s().getDraft("b")?.text).toBe("two");
  });

  it("drops an entry when a patch makes it no longer meaningful", () => {
    const { useStore } = build("k_prune_on_write");
    const s = () => useStore.getState();
    s().setDraft("a", { text: "hello" });
    s().setDraft("a", { text: "   " });

    expect(s().getDraft("a")).toBeUndefined();
    expect(Object.keys(s().drafts)).toEqual([]);
  });

  it("does not share nested references with emptyData", () => {
    const { useStore } = build("k_nested");
    useStore.getState().setDraft("a", { text: "t" });
    useStore.getState().getDraft("a")!.tags.push("leak");
    expect(EMPTY.tags).toEqual([]);
  });

  it("persists and rehydrates entries across store instances", () => {
    const first = build("k_persist");
    first.useStore.getState().setDraft("a", { text: "survives" });

    // New instance over the same storage = a reload / remount of the module.
    __clearDraftCleanupRegistryForTest();
    const second = build("k_persist", first.storage);

    expect(second.useStore.getState().getDraft("a")?.text).toBe("survives");
  });

  it("drops entries older than 30 days and non-meaningful entries on init", () => {
    const now = Date.now();
    const seed = memoryStorage({
      k_ttl: JSON.stringify({
        state: {
          drafts: {
            fresh: { data: { text: "keep", tags: [] }, updatedAt: now - DAY_MS },
            stale: { data: { text: "old", tags: [] }, updatedAt: now - 31 * DAY_MS },
            blank: { data: { text: " ", tags: [] }, updatedAt: now },
          },
        },
        version: 0,
      }),
    });

    const { useStore } = build("k_ttl", seed);

    expect(Object.keys(useStore.getState().drafts)).toEqual(["fresh"]);
  });

  it("self-registers for cleanup; reset wipes memory, workspace clear wipes storage", () => {
    const { useStore, storage } = build("k_register", memoryStorage(), {
      workspaceScoped: true,
    });
    expect(registeredDraftKeys().workspaceScoped).toContain("k_register");

    setCurrentWorkspace("ws-a", "id-a");
    useStore.getState().setDraft("a", { text: "mine" });
    expect(storage.values.has("k_register:ws-a")).toBe(true);

    clearRegisteredWorkspaceDrafts(storage, "ws-a");
    expect(storage.values.has("k_register:ws-a")).toBe(false);

    useStore.getState().setDraft("a", { text: "again" });
    resetAllRegisteredDrafts(); // logout
    expect(useStore.getState().hasDraft("a")).toBe(false);
  });

  it("scopes entries per workspace and rehydrates on workspace switch", async () => {
    setCurrentWorkspace("ws-a", "id-a");
    const { useStore } = build("k_ws", memoryStorage(), { workspaceScoped: true });
    await flush();
    useStore.getState().setDraft("issue-1", { text: "in A" });

    setCurrentWorkspace("ws-b", "id-b");
    await flush();
    expect(useStore.getState().hasDraft("issue-1")).toBe(false);
    useStore.getState().setDraft("issue-1", { text: "in B" });

    setCurrentWorkspace("ws-a", "id-a");
    await flush();
    expect(useStore.getState().getDraft("issue-1")?.text).toBe("in A");
  });
});
