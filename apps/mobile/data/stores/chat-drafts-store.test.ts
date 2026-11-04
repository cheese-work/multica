import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// In-memory stand-in for the native AsyncStorage module. It outlives
// vi.resetModules(), exactly like the disk outlives an app process.
const disk = vi.hoisted(() => new Map<string, string>());
vi.mock("@react-native-async-storage/async-storage", () => ({
  default: {
    getItem: async (k: string) => disk.get(k) ?? null,
    setItem: async (k: string, v: string) => void disk.set(k, v),
    removeItem: async (k: string) => void disk.delete(k),
    getAllKeys: async () => [...disk.keys()],
    multiGet: async (ks: string[]) =>
      ks.map((k) => [k, disk.get(k) ?? null] as [string, string | null]),
    multiRemove: async (ks: string[]) => void ks.forEach((k) => disk.delete(k)),
  },
}));

// A cold start: new JS module graph, same disk.
async function coldStart() {
  vi.resetModules();
  const store = await import("./chat-drafts-store");
  const persistence = await import("../draft-persistence");
  return { ...store, ...persistence };
}

describe("chat drafts survive an app cold start", () => {
  beforeEach(() => {
    disk.clear();
    vi.useFakeTimers();
  });
  afterEach(() => vi.useRealTimers());

  it("restores typed text for a session", async () => {
    const run1 = await coldStart();
    await run1.useChatDraftsStore.getState().hydrate("ws1");
    run1.useChatDraftsStore.getState().setDraft("session-a", "half-typed note");
    await vi.advanceTimersByTimeAsync(1000);

    const run2 = await coldStart();
    await run2.useChatDraftsStore.getState().hydrate("ws1");
    expect(run2.useChatDraftsStore.getState().drafts["session-a"]).toBe(
      "half-typed note",
    );
  });

  it("restores the new-chat draft", async () => {
    const run1 = await coldStart();
    await run1.useChatDraftsStore.getState().hydrate("ws1");
    run1.useChatDraftsStore.getState().setDraft(run1.DRAFT_NEW_SESSION, "idea");
    await vi.advanceTimersByTimeAsync(1000);

    const run2 = await coldStart();
    await run2.useChatDraftsStore.getState().hydrate("ws1");
    expect(
      run2.useChatDraftsStore.getState().drafts[run2.DRAFT_NEW_SESSION],
    ).toBe("idea");
  });

  it("does not restore a draft cleared by a successful send", async () => {
    const run1 = await coldStart();
    const s = run1.useChatDraftsStore.getState();
    await s.hydrate("ws1");
    s.setDraft("session-a", "sent text");
    await vi.advanceTimersByTimeAsync(1000);
    s.clearDraft("session-a");
    await vi.advanceTimersByTimeAsync(1000);

    const run2 = await coldStart();
    await run2.useChatDraftsStore.getState().hydrate("ws1");
    expect(run2.useChatDraftsStore.getState().drafts).toEqual({});
  });

  it("moves the new-chat draft onto the created session on disk", async () => {
    const run1 = await coldStart();
    const s = run1.useChatDraftsStore.getState();
    await s.hydrate("ws1");
    s.setDraft(run1.DRAFT_NEW_SESSION, "first message");
    await vi.advanceTimersByTimeAsync(1000);
    s.promoteNewDraft("session-new");
    await vi.advanceTimersByTimeAsync(1000);

    const run2 = await coldStart();
    await run2.useChatDraftsStore.getState().hydrate("ws1");
    expect(run2.useChatDraftsStore.getState().drafts).toEqual({
      "session-new": "first message",
    });
  });

  it("scopes drafts per workspace", async () => {
    const run1 = await coldStart();
    await run1.useChatDraftsStore.getState().hydrate("ws1");
    run1.useChatDraftsStore.getState().setDraft("session-a", "ws1 text");
    await vi.advanceTimersByTimeAsync(1000);

    const run2 = await coldStart();
    await run2.useChatDraftsStore.getState().hydrate("ws2");
    expect(run2.useChatDraftsStore.getState().drafts).toEqual({});
    await run2.useChatDraftsStore.getState().hydrate("ws1");
    expect(run2.useChatDraftsStore.getState().drafts["session-a"]).toBe(
      "ws1 text",
    );
  });

  it("keeps text typed while hydration is still reading disk", async () => {
    const run1 = await coldStart();
    await run1.useChatDraftsStore.getState().hydrate("ws1");
    run1.useChatDraftsStore.getState().setDraft("session-a", "old");
    await vi.advanceTimersByTimeAsync(1000);

    const run2 = await coldStart();
    const hydrating = run2.useChatDraftsStore.getState().hydrate("ws1");
    run2.useChatDraftsStore.getState().setDraft("session-a", "newer");
    await hydrating;
    expect(run2.useChatDraftsStore.getState().drafts["session-a"]).toBe("newer");
  });

  it("clears memory and disk on logout", async () => {
    const run1 = await coldStart();
    await run1.useChatDraftsStore.getState().hydrate("ws1");
    run1.useChatDraftsStore.getState().setDraft("session-a", "private");
    await vi.advanceTimersByTimeAsync(1000);

    await run1.clearAllDrafts();
    expect(run1.useChatDraftsStore.getState().drafts).toEqual({});

    const run2 = await coldStart();
    await run2.useChatDraftsStore.getState().hydrate("ws1");
    expect(run2.useChatDraftsStore.getState().drafts).toEqual({});
  });
});
