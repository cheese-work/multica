import AsyncStorage from "@react-native-async-storage/async-storage";
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

  it("logout with a failed disk delete reports it and still empties memory", async () => {
    const run1 = await coldStart();
    await run1.useChatDraftsStore.getState().hydrate("user1:ws1");
    run1.useChatDraftsStore.getState().setDraft("session-a", "private");
    await vi.advanceTimersByTimeAsync(1000);

    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const realMultiRemove = AsyncStorage.multiRemove;
    AsyncStorage.multiRemove = async () => {
      throw new Error("disk full");
    };
    await expect(run1.clearAllDrafts()).rejects.toThrow("disk full");
    AsyncStorage.multiRemove = realMultiRemove;
    warn.mockRestore();
    expect(run1.useChatDraftsStore.getState().drafts).toEqual({});

    // The drafts are still on disk, but another account's scope never sees them.
    const run2 = await coldStart();
    await run2.useChatDraftsStore.getState().hydrate("user2:ws1");
    expect(run2.useChatDraftsStore.getState().drafts).toEqual({});
  });

  it("keeps text typed after the composer's optimistic clear on submit", async () => {
    const run1 = await coldStart();
    const s = run1.useChatDraftsStore.getState();
    await s.hydrate("user1:ws1");
    s.setDraft("session-a", "first message");
    s.setDraft("session-a", ""); // composer empties the input on submit
    s.setDraft("session-a", "second message"); // typed while the send is pending
    // A successful send must not touch the draft: nothing is called here.
    await vi.advanceTimersByTimeAsync(1000);

    const run2 = await coldStart();
    await run2.useChatDraftsStore.getState().hydrate("user1:ws1");
    expect(run2.useChatDraftsStore.getState().drafts["session-a"]).toBe(
      "second message",
    );
  });

  describe("a send is in flight", () => {
    // The composer empties the input on submit; the draft must stay on disk
    // until the send succeeds. `begin` mirrors the composer's submit order.
    function submit(store: { beginSend: (k: string, t: string) => string; setDraft: (k: string, t: string) => void }, key: string, text: string) {
      const token = store.beginSend(key, text);
      store.setDraft(key, "");
      return token;
    }

    it("restores the submitted text after a restart mid-send", async () => {
      const run1 = await coldStart();
      const s = run1.useChatDraftsStore.getState();
      await s.hydrate("u1:ws1");
      s.setDraft("session-a", "submitted");
      submit(s, "session-a", "submitted");
      await vi.advanceTimersByTimeAsync(1000);

      const run2 = await coldStart();
      await run2.useChatDraftsStore.getState().hydrate("u1:ws1");
      expect(run2.useChatDraftsStore.getState().drafts["session-a"]).toBe(
        "submitted",
      );
    });

    it("restores submitted text and later edits together after a restart", async () => {
      const run1 = await coldStart();
      const s = run1.useChatDraftsStore.getState();
      await s.hydrate("u1:ws1");
      s.setDraft("session-a", "submitted");
      submit(s, "session-a", "submitted");
      s.setDraft("session-a", "typed later");
      await vi.advanceTimersByTimeAsync(1000);

      const run2 = await coldStart();
      await run2.useChatDraftsStore.getState().hydrate("u1:ws1");
      expect(run2.useChatDraftsStore.getState().drafts["session-a"]).toBe(
        "submitted\ntyped later",
      );
    });

    it("drops the submitted text on success and keeps later edits", async () => {
      const run1 = await coldStart();
      const s = run1.useChatDraftsStore.getState();
      await s.hydrate("u1:ws1");
      s.setDraft("session-a", "submitted");
      const token = submit(s, "session-a", "submitted");
      s.setDraft("session-a", "typed later");
      s.finishSend(token, true);
      await vi.advanceTimersByTimeAsync(1000);

      const run2 = await coldStart();
      await run2.useChatDraftsStore.getState().hydrate("u1:ws1");
      expect(run2.useChatDraftsStore.getState().drafts["session-a"]).toBe(
        "typed later",
      );
    });

    it("clears everything on success when nothing was typed", async () => {
      const run1 = await coldStart();
      const s = run1.useChatDraftsStore.getState();
      await s.hydrate("u1:ws1");
      s.setDraft("session-a", "submitted");
      const token = submit(s, "session-a", "submitted");
      s.finishSend(token, true);
      await vi.advanceTimersByTimeAsync(1000);

      const run2 = await coldStart();
      await run2.useChatDraftsStore.getState().hydrate("u1:ws1");
      expect(run2.useChatDraftsStore.getState().drafts).toEqual({});
    });

    it("does not duplicate the text when a failed send is restored", async () => {
      const run1 = await coldStart();
      const s = run1.useChatDraftsStore.getState();
      await s.hydrate("u1:ws1");
      s.setDraft("session-a", "submitted");
      const token = submit(s, "session-a", "submitted");
      s.setDraft("session-a", "submitted"); // composer restores on failure
      s.finishSend(token, false);
      await vi.advanceTimersByTimeAsync(1000);

      const run2 = await coldStart();
      await run2.useChatDraftsStore.getState().hydrate("u1:ws1");
      expect(run2.useChatDraftsStore.getState().drafts["session-a"]).toBe(
        "submitted",
      );
    });

    it("follows the new-chat draft onto the created session", async () => {
      const run1 = await coldStart();
      const s = run1.useChatDraftsStore.getState();
      await s.hydrate("u1:ws1");
      s.setDraft(run1.DRAFT_NEW_SESSION, "first message");
      const token = submit(s, run1.DRAFT_NEW_SESSION, "first message");
      s.promoteNewDraft("session-new");
      await vi.advanceTimersByTimeAsync(1000);

      const midSend = await coldStart();
      await midSend.useChatDraftsStore.getState().hydrate("u1:ws1");
      expect(midSend.useChatDraftsStore.getState().drafts).toEqual({
        "session-new": "first message",
      });

      s.finishSend(token, true);
      await vi.advanceTimersByTimeAsync(1000);
      const run3 = await coldStart();
      await run3.useChatDraftsStore.getState().hydrate("u1:ws1");
      expect(run3.useChatDraftsStore.getState().drafts).toEqual({});
    });
  });
});

describe("a send is in flight while the workspace changes", () => {
  beforeEach(() => {
    disk.clear();
    vi.useFakeTimers();
  });
  afterEach(() => vi.useRealTimers());

  // Workspace A new-chat send is open, then the user opens workspace B and
  // types in B's new chat.
  async function switchDuringSend() {
    const run = await coldStart();
    const s = run.useChatDraftsStore.getState();
    await s.hydrate("u1:wsA");
    s.setDraft(run.DRAFT_NEW_SESSION, "from A");
    const token = s.beginSend(run.DRAFT_NEW_SESSION, "from A");
    s.setDraft(run.DRAFT_NEW_SESSION, "");
    await s.hydrate("u1:wsB");
    s.setDraft(run.DRAFT_NEW_SESSION, "from B");
    await vi.advanceTimersByTimeAsync(1000);
    return { run, s, token };
  }

  async function draftsAfterRestart(scope: string, newKey: string) {
    const next = await coldStart();
    await next.useChatDraftsStore.getState().hydrate(scope);
    return next.useChatDraftsStore.getState().drafts[newKey];
  }

  it("keeps the other workspace's submitted text out of the new chat", async () => {
    const { run } = await switchDuringSend();
    expect(
      run.useChatDraftsStore.getState().drafts[run.DRAFT_NEW_SESSION],
    ).toBe("from B");
    expect(await draftsAfterRestart("u1:wsB", run.DRAFT_NEW_SESSION)).toBe(
      "from B",
    );
    expect(await draftsAfterRestart("u1:wsA", run.DRAFT_NEW_SESSION)).toBe(
      "from A",
    );
  });

  it("a late success settles the original workspace only", async () => {
    const { run, s, token } = await switchDuringSend();
    s.finishSend(token, true);
    await vi.advanceTimersByTimeAsync(1000);
    expect(
      run.useChatDraftsStore.getState().drafts[run.DRAFT_NEW_SESSION],
    ).toBe("from B");
    expect(await draftsAfterRestart("u1:wsB", run.DRAFT_NEW_SESSION)).toBe(
      "from B",
    );
    expect(
      await draftsAfterRestart("u1:wsA", run.DRAFT_NEW_SESSION),
    ).toBeUndefined();
  });

  it("a late failure keeps the text in the original workspace only", async () => {
    const { run, s, token } = await switchDuringSend();
    s.finishSend(token, false);
    await vi.advanceTimersByTimeAsync(1000);
    expect(
      run.useChatDraftsStore.getState().drafts[run.DRAFT_NEW_SESSION],
    ).toBe("from B");
    expect(await draftsAfterRestart("u1:wsB", run.DRAFT_NEW_SESSION)).toBe(
      "from B",
    );
    expect(await draftsAfterRestart("u1:wsA", run.DRAFT_NEW_SESSION)).toBe(
      "from A",
    );
  });

  it("ignores an input write that belongs to the workspace that was left", async () => {
    const { run, s } = await switchDuringSend();
    // The old composer restores its text after a failure, with the scope it
    // was rendered in.
    s.setDraft(run.DRAFT_NEW_SESSION, "stale restore", "u1:wsA");
    await vi.advanceTimersByTimeAsync(1000);
    expect(
      run.useChatDraftsStore.getState().drafts[run.DRAFT_NEW_SESSION],
    ).toBe("from B");
    expect(await draftsAfterRestart("u1:wsB", run.DRAFT_NEW_SESSION)).toBe(
      "from B",
    );
  });

  it("a late session creation from the left workspace leaves the other workspace alone", async () => {
    const { run, s, token } = await switchDuringSend();
    // A's session is created only now; promotion carries A's scope.
    s.promoteNewDraft("session-a1", "u1:wsA");
    await vi.advanceTimersByTimeAsync(1000);

    const state = run.useChatDraftsStore.getState();
    expect(state.drafts[run.DRAFT_NEW_SESSION]).toBe("from B");
    expect(state.drafts["session-a1"]).toBeUndefined();
    expect(await draftsAfterRestart("u1:wsB", run.DRAFT_NEW_SESSION)).toBe(
      "from B",
    );
    expect(await draftsAfterRestart("u1:wsB", "session-a1")).toBeUndefined();
    // A's open send moved onto the created session, not B's new chat.
    expect(await draftsAfterRestart("u1:wsA", "session-a1")).toBe("from A");
    expect(
      await draftsAfterRestart("u1:wsA", run.DRAFT_NEW_SESSION),
    ).toBeUndefined();

    s.finishSend(token, true);
    await vi.advanceTimersByTimeAsync(1000);
    expect(
      await draftsAfterRestart("u1:wsA", "session-a1"),
    ).toBeUndefined();
    expect(await draftsAfterRestart("u1:wsB", run.DRAFT_NEW_SESSION)).toBe(
      "from B",
    );
  });

  it("a late session creation does not repoint a send open in the other workspace", async () => {
    const { run, s, token } = await switchDuringSend();
    // B starts its own new-chat send while A's creation is still pending.
    const tokenB = s.beginSend(run.DRAFT_NEW_SESSION, "from B");
    s.setDraft(run.DRAFT_NEW_SESSION, "");
    s.promoteNewDraft("session-a1", "u1:wsA");
    s.setDraft(run.DRAFT_NEW_SESSION, "from B"); // composer restores on failure
    s.finishSend(tokenB, false);
    s.finishSend(token, true);
    await vi.advanceTimersByTimeAsync(1000);
    expect(
      run.useChatDraftsStore.getState().drafts[run.DRAFT_NEW_SESSION],
    ).toBe("from B");
    expect(await draftsAfterRestart("u1:wsB", run.DRAFT_NEW_SESSION)).toBe(
      "from B",
    );
    expect(await draftsAfterRestart("u1:wsB", "session-a1")).toBeUndefined();
  });
});

