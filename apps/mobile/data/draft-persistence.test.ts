import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  createDraftPersistence,
  DRAFT_TTL_MS,
  type DraftStorage,
} from "./draft-persistence";

function memoryStorage(): DraftStorage & { data: Map<string, string> } {
  const data = new Map<string, string>();
  return {
    data,
    getItem: async (k) => data.get(k) ?? null,
    setItem: async (k, v) => void data.set(k, v),
    removeItem: async (k) => void data.delete(k),
    getAllKeys: async () => [...data.keys()],
    multiGet: async (ks) => ks.map((k) => [k, data.get(k) ?? null] as [string, string | null]),
    multiRemove: async (ks) => void ks.forEach((k) => data.delete(k)),
  };
}

describe("draft persistence", () => {
  let clock = 1_000_000;
  const now = () => clock;
  beforeEach(() => {
    clock = 1_000_000;
    vi.useFakeTimers();
  });
  afterEach(() => vi.useRealTimers());

  it("restores a saved draft on a fresh instance (cold start)", async () => {
    const storage = memoryStorage();
    const first = createDraftPersistence({ storage, now });
    first.save("ws1", "chat:a", "hello");
    await vi.advanceTimersByTimeAsync(1000);

    const afterRestart = createDraftPersistence({ storage, now });
    expect(await afterRestart.loadAll("ws1", "chat:")).toEqual({
      "chat:a": "hello",
    });
  });

  it("debounces: rapid saves produce one write of the latest value", async () => {
    const storage = memoryStorage();
    const setItem = vi.spyOn(storage, "setItem");
    const p = createDraftPersistence({ storage, now, debounceMs: 500 });
    p.save("ws1", "k", "a");
    p.save("ws1", "k", "ab");
    p.save("ws1", "k", "abc");
    await vi.advanceTimersByTimeAsync(499);
    expect(setItem).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(setItem).toHaveBeenCalledTimes(1);
    expect(await p.loadAll("ws1", "")).toEqual({ k: "abc" });
  });

  it("flush writes pending drafts immediately", async () => {
    const storage = memoryStorage();
    const p = createDraftPersistence({ storage, now });
    p.save("ws1", "k", "x");
    await p.flush();
    expect(storage.data.size).toBe(1);
  });

  it("drops drafts older than 30 days and deletes them from disk", async () => {
    const storage = memoryStorage();
    const p = createDraftPersistence({ storage, now });
    p.save("ws1", "old", "stale");
    await p.flush();
    clock += DRAFT_TTL_MS - 1;
    p.save("ws1", "fresh", "ok");
    await p.flush();
    clock += 2; // "old" is now past the TTL; "fresh" is not

    expect(await p.loadAll("ws1", "")).toEqual({ fresh: "ok" });
    expect(storage.data.size).toBe(1);
  });

  it("sweepExpired removes expired drafts of every workspace", async () => {
    const storage = memoryStorage();
    const p = createDraftPersistence({ storage, now });
    p.save("ws1", "a", "1");
    p.save("ws2", "b", "2");
    await p.flush();
    clock += DRAFT_TTL_MS + 1;
    await p.sweepExpired();
    expect(storage.data.size).toBe(0);
  });

  it("keeps workspaces apart", async () => {
    const storage = memoryStorage();
    const p = createDraftPersistence({ storage, now });
    p.save("ws1", "chat:a", "one");
    p.save("ws2", "chat:a", "two");
    await p.flush();
    expect(await p.loadAll("ws1", "chat:")).toEqual({ "chat:a": "one" });
    expect(await p.loadAll("ws2", "chat:")).toEqual({ "chat:a": "two" });
  });

  it("remove deletes the stored draft and cancels a pending write", async () => {
    const storage = memoryStorage();
    const p = createDraftPersistence({ storage, now });
    p.save("ws1", "k", "x");
    await p.flush();
    p.save("ws1", "k", "xy"); // pending
    p.remove("ws1", "k");
    await vi.advanceTimersByTimeAsync(2000);
    expect(await p.loadAll("ws1", "")).toEqual({});
    expect(storage.data.size).toBe(0);
  });

  it("clearAll wipes every draft and drops pending writes (logout)", async () => {
    const storage = memoryStorage();
    storage.data.set("unrelated", "keep");
    const p = createDraftPersistence({ storage, now });
    p.save("ws1", "a", "1");
    await p.flush();
    p.save("ws2", "b", "2"); // still pending
    await p.clearAll();
    await vi.advanceTimersByTimeAsync(2000);
    expect([...storage.data.keys()]).toEqual(["unrelated"]);
  });

  it("ignores corrupt entries instead of throwing", async () => {
    const storage = memoryStorage();
    const p = createDraftPersistence({ storage, now });
    p.save("ws1", "good", "ok");
    await p.flush();
    const [goodKey] = [...storage.data.keys()];
    storage.data.set(goodKey.replace("good", "bad"), "{not json");
    expect(await p.loadAll("ws1", "")).toEqual({ good: "ok" });
  });

  it("clearAll rejects when the disk delete fails, and later work still runs", async () => {
    const storage = memoryStorage();
    const p = createDraftPersistence({ storage, now });
    p.save("ws1", "a", "1");
    await p.flush();
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const multiRemove = vi
      .spyOn(storage, "multiRemove")
      .mockRejectedValueOnce(new Error("disk full"));

    await expect(p.clearAll()).rejects.toThrow("disk full");
    expect(multiRemove).toHaveBeenCalledTimes(1);

    // The queue is not stuck: a retry succeeds.
    await p.clearAll();
    expect(storage.data.size).toBe(0);
    warn.mockRestore();
  });
});
