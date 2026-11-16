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
import { defaultStorage } from "../platform/storage";
import { currentDraftOrigin, DRAFT_SESSION_KEY } from "./cleanup-registry";
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

  it("a write right after a workspace switch, before rehydration, does not clobber the new workspace", async () => {
    setCurrentWorkspace("ws-a", "id-a");
    const { useStore, storage } = build("k_ws_race", memoryStorage(), { workspaceScoped: true });
    await flush();
    useStore.getState().setDraft("issue-1", { text: "in A" });
    setCurrentWorkspace("ws-b", "id-b");
    await flush();
    useStore.getState().setDraft("issue-2", { text: "in B" });

    // Switch back to A and write before the queued rehydration runs.
    setCurrentWorkspace("ws-a", "id-a");
    useStore.getState().setDraft("issue-3", { text: "A again" });
    await flush();

    // The stored key carries the session token after a logout elsewhere in this file.
    const persisted = (slug: string) => {
      const key = [...storage.values.keys()].find((k) => k === `k_ws_race:${slug}` || k.startsWith(`k_ws_race:${slug}#`))!;
      return Object.keys(JSON.parse(storage.values.get(key)!).state.drafts).sort();
    };
    expect(persisted("ws-a")).toEqual(["issue-1", "issue-3"]);
    expect(persisted("ws-b")).toEqual(["issue-2"]);
    expect(Object.keys(useStore.getState().drafts).sort()).toEqual(["issue-1", "issue-3"]);
  });

  it("reads right after a workspace switch, before rehydration, never show the old workspace's drafts", async () => {
    setCurrentWorkspace("ws-a", "id-a");
    const { useStore } = build("k_ws_read", memoryStorage(), { workspaceScoped: true });
    await flush();
    useStore.getState().setDraft("k", { text: "in A" });
    setCurrentWorkspace("ws-b", "id-b");
    await flush();
    useStore.getState().setDraft("k", { text: "in B" });

    // Memory still holds B's map while the slug is A (rehydration is queued):
    // reads must be empty, not B's draft.
    setCurrentWorkspace("ws-a", "id-a");
    expect(useStore.getState().getDraft("k")).toBeUndefined();
    expect(useStore.getState().hasDraft("k")).toBe(false);
    await flush();
    expect(useStore.getState().getDraft("k")?.text).toBe("in A");

    setCurrentWorkspace("ws-c", "id-c"); // no draft in C
    expect(useStore.getState().getDraft("k")).toBeUndefined();
    await flush();
    expect(useStore.getState().hasDraft("k")).toBe(false);
  });
  it("logout between the storage read and the merge: the old payload is rejected, not adopted by the new session", () => {
    const storage = memoryStorage();
    const first = build("k_logout_race", storage).useStore;
    first.getState().setDraft("a", { text: "old user note" });

    // Another tab logs out right after this load read the stored payload.
    const realGet = storage.getItem;
    let armed = true;
    storage.getItem = (k) => {
      const value = realGet(k);
      console.log("READ", k);
      if (armed && k.startsWith("k_logout_race") && value) {
        armed = false;
        resetAllRegisteredDrafts();
      }
      return value;
    };

    const { useStore } = build("k_logout_race", storage);

    expect(useStore.getState().getDraft("a")).toBeUndefined();
    useStore.getState().setDraft("b", { text: "new user note" });
    const raw = [...storage.values.values()].join("\n");
    expect(raw).not.toContain("old user note");
    expect(raw).toContain("new user note");
  });

  // Another tab ends the session of ws-a right after the session check of a
  // delayed op, before its next session read. `afterEnd` runs once the new token exists.
  const endSessionAfterFirstRead = (storage: StorageAdapter, afterEnd: () => void = () => {}) => {
    const realGet = defaultStorage.getItem;
    let state: "armed" | "fire" | "done" = "armed";
    const end = () => {
      state = "done";
      clearRegisteredWorkspaceDrafts(storage, "ws-a");
      afterEnd();
    };
    defaultStorage.getItem = (k) => {
      if (state === "fire") end(); // the read after the check: the session ends right before it
      if (state === "armed" && k === `${DRAFT_SESSION_KEY}:ws-a`) state = "fire";
      return realGet(k);
    };
    return () => {
      defaultStorage.getItem = realGet;
      // An op that reads the token once never reaches the second read: end the session after it.
      if (state !== "done") end();
    };
  };

  it("delayed write: session ends between the check and key selection; the old note never lands under the new token", async () => {
    setCurrentWorkspace("ws-a", "id-a");
    const { useStore, storage } = build("k_key_race_set", memoryStorage(), { workspaceScoped: true });
    await flush();
    const origin = currentDraftOrigin("ws-a"); // editor scheduled its flush in ws-a
    setCurrentWorkspace("ws-b", "id-b");
    await flush();

    const restore = endSessionAfterFirstRead(storage);
    try {
      useStore.getState().setDraftIn(origin, "issue-1", { text: "old private note" });
    } finally {
      restore();
    }

    const newToken = currentDraftOrigin("ws-a").session;
    expect(newToken).not.toBe(origin.session);
    const leaked = [...storage.values.entries()].filter(
      ([k, v]) => k.endsWith(`#${newToken}`) && v.includes("old private note"),
    );
    expect(leaked).toEqual([]);
  });

  it("delayed clear: session ends between the check and key selection; the new session's draft survives", async () => {
    setCurrentWorkspace("ws-a", "id-a");
    const { useStore, storage } = build("k_key_race_clear", memoryStorage(), { workspaceScoped: true });
    await flush();
    const origin = currentDraftOrigin("ws-a");
    setCurrentWorkspace("ws-b", "id-b");
    await flush();

    const newKey = () => `k_key_race_clear:ws-a#${currentDraftOrigin("ws-a").session}`;
    const restore = endSessionAfterFirstRead(storage, () =>
      storage.values.set(
        newKey(),
        JSON.stringify({ state: { drafts: { "issue-1": { data: { text: "new note", tags: [] }, updatedAt: Date.now() } } }, version: 0 }),
      ),
    );
    try {
      useStore.getState().clearDraftIn(origin, "issue-1");
    } finally {
      restore();
    }

    expect(storage.values.get(newKey())).toContain("new note");
  });
});
