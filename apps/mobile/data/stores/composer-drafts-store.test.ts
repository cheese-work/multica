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
async function coldStart(scope = "u1:ws1") {
  vi.resetModules();
  const store = (await import("./composer-drafts-store")).useComposerDraftsStore;
  const persistence = await import("../draft-persistence");
  await store.getState().hydrate(scope);
  return { store, persistence };
}

const settle = () => vi.advanceTimersByTimeAsync(1000);

const reply = { commentId: "c1", actorName: "Ada", preview: "hello" };
const mention = { type: "agent" as const, id: "a1", name: "Bot" };
const upload = {
  localId: "l1",
  localUri: "file:///cache/a.png",
  filename: "a.png",
  mimeType: "image/png",
  status: "completed" as const,
  id: "att1",
  url: "mc://file/att1",
  downloadUrl: "/api/attachments/att1/download",
};

describe("composer drafts survive unmount, route change and cold start", () => {
  beforeEach(() => {
    disk.clear();
    vi.useFakeTimers();
  });
  afterEach(() => vi.useRealTimers());

  it("comment composer: typed text comes back per issue", async () => {
    const run1 = await coldStart();
    run1.store.getState().patch("comment:i1", { text: "half-typed" });
    run1.store.getState().patch("comment:i2", { text: "other issue" });
    await settle();

    const run2 = await coldStart();
    expect(run2.store.getState().drafts["comment:i1"]?.text).toBe("half-typed");
    expect(run2.store.getState().drafts["comment:i2"]?.text).toBe("other issue");
  });

  it("comment composer: the reply target comes back", async () => {
    const run1 = await coldStart();
    run1.store.getState().patch("comment:i1", { reply });
    await settle();

    const run2 = await coldStart();
    expect(run2.store.getState().drafts["comment:i1"]?.reply).toEqual(reply);
  });

  it("mention chips come back", async () => {
    const run1 = await coldStart();
    run1.store.getState().patch("comment:i1", { mentions: [mention] });
    await settle();

    const run2 = await coldStart();
    expect(run2.store.getState().drafts["comment:i1"]?.mentions).toEqual([
      mention,
    ]);
  });

  it("chat composer: uploaded attachments come back, an in-flight one does not", async () => {
    const run1 = await coldStart();
    run1.store.getState().patch("chat:s1", {
      attachments: [upload, { ...upload, localId: "l2", status: "uploading", id: undefined }],
    });
    await settle();

    const run2 = await coldStart();
    expect(run2.store.getState().drafts["chat:s1"]?.attachments).toEqual([upload]);
  });

  it("an upload that finishes after the composer unmounted still lands", async () => {
    const run1 = await coldStart();
    const s = run1.store.getState();
    s.patch("comment:i1", {
      attachments: [{ ...upload, status: "uploading", id: undefined }],
    });
    s.patchAttachments("comment:i1", (prev) =>
      prev.map((a) => ({ ...a, status: "completed" as const, id: "att1" })),
    );
    await settle();

    const run2 = await coldStart();
    expect(run2.store.getState().drafts["comment:i1"]?.attachments[0]?.id).toBe("att1");
  });

  it("a successful send clears the draft", async () => {
    const run1 = await coldStart();
    const s = run1.store.getState();
    s.patch("comment:i1", { text: "sent", mentions: [mention], reply });
    await settle();
    const sendScope = s.beginSend("comment:i1");
    s.patch("comment:i1", { text: "", mentions: [], attachments: [], reply: null });
    s.finishSend("comment:i1", sendScope, true);
    await settle();

    const run2 = await coldStart();
    expect(run2.store.getState().drafts).toEqual({});
  });

  it("a failed send keeps the draft", async () => {
    const run1 = await coldStart();
    const s = run1.store.getState();
    s.patch("comment:i1", { text: "refused", mentions: [mention] });
    await settle();
    const sendScope = s.beginSend("comment:i1");
    s.patch("comment:i1", { text: "", mentions: [], attachments: [], reply: null });
    // the composer restores the snapshot on rejection
    s.patch("comment:i1", { text: "refused", mentions: [mention] });
    s.finishSend("comment:i1", sendScope, false);
    await settle();

    const run2 = await coldStart();
    expect(run2.store.getState().drafts["comment:i1"]?.text).toBe("refused");
  });

  it("the app killed mid-send keeps the submitted draft", async () => {
    const run1 = await coldStart();
    const s = run1.store.getState();
    s.patch("comment:i1", { text: "in flight" });
    s.beginSend("comment:i1");
    s.patch("comment:i1", { text: "" });
    await settle();

    const run2 = await coldStart();
    expect(run2.store.getState().drafts["comment:i1"]?.text).toBe("in flight");
  });

  it("drafts stay in their own user + workspace scope", async () => {
    const run1 = await coldStart("u1:ws1");
    run1.store.getState().patch("comment:i1", { text: "ws1 only" });
    await settle();

    const other = await coldStart("u1:ws2");
    expect(other.store.getState().drafts).toEqual({});
  });

  it("logout empties memory and disk", async () => {
    const run1 = await coldStart();
    run1.store.getState().patch("comment:i1", { text: "gone after logout" });
    await settle();
    await run1.persistence.clearAllDrafts();

    expect(run1.store.getState().drafts).toEqual({});
    const run2 = await coldStart();
    expect(run2.store.getState().drafts).toEqual({});
  });

  it("editing before the disk drafts load keeps the whole saved draft", async () => {
    const run1 = await coldStart();
    run1.store.getState().patch("comment:i1", {
      text: "hello",
      reply,
      mentions: [mention],
      attachments: [upload],
    });
    await settle();

    vi.resetModules();
    const store = (await import("./composer-drafts-store")).useComposerDraftsStore;
    const loading = store.getState().hydrate("u1:ws1");
    store.getState().patch("comment:i1", { text: "x" });
    store.getState().patch("comment:i1", { mentions: [] });
    store
      .getState()
      .patchAttachments("comment:i1", (prev) => [
        ...prev,
        { ...upload, localId: "l2", id: "att2" },
      ]);
    await loading;
    await settle();

    const expected = {
      text: "hello",
      mentions: [mention],
      reply,
      attachments: [upload, { ...upload, localId: "l2", id: "att2" }],
    };
    expect(store.getState().drafts["comment:i1"]).toEqual(expected);
    const run2 = await coldStart();
    expect(run2.store.getState().drafts["comment:i1"]).toEqual(expected);
  });

  it("a failed send that settles after a workspace switch stays in its own workspace", async () => {
    const run1 = await coldStart("u1:ws1");
    const s = run1.store.getState();
    s.patch("comment:i1", { text: "refused", mentions: [mention] });
    const sendScope = s.beginSend("comment:i1");
    s.patch("comment:i1", { text: "", mentions: [] });
    await run1.store.getState().hydrate("u1:ws2");

    // the composer restores its snapshot for the scope the send started in
    s.patch("comment:i1", { text: "refused", mentions: [mention] }, sendScope);
    s.finishSend("comment:i1", sendScope, false);
    await settle();

    expect(run1.store.getState().drafts).toEqual({});
    const back = await coldStart("u1:ws1");
    expect(back.store.getState().drafts["comment:i1"]?.text).toBe("refused");
  });

  it("a successful send that settles after a workspace switch clears its own workspace", async () => {
    const run1 = await coldStart("u1:ws1");
    const s = run1.store.getState();
    s.patch("comment:i1", { text: "sent" });
    const sendScope = s.beginSend("comment:i1");
    s.patch("comment:i1", { text: "" });
    await run1.store.getState().hydrate("u1:ws2");
    s.finishSend("comment:i1", sendScope, true);
    await settle();

    const back = await coldStart("u1:ws1");
    expect(back.store.getState().drafts).toEqual({});
  });

  it("an upload that finishes after a workspace switch is dropped", async () => {
    const run1 = await coldStart("u1:ws1");
    const s = run1.store.getState();
    const uploadScope = s.scopeId;
    await run1.store.getState().hydrate("u1:ws2");
    s.patchAttachments("comment:i1", () => [upload], uploadScope);
    await settle();

    expect(run1.store.getState().drafts).toEqual({});
  });

  it("deleting a workspace clears its drafts and leaves the others", async () => {
    const other = await coldStart("u1:ws2");
    other.store.getState().patch("comment:i1", { text: "ws2 draft" });
    await settle();
    const run1 = await coldStart("u1:ws1");
    run1.store.getState().patch("comment:i1", { text: "ws1 draft" });
    await settle();

    await run1.persistence.clearScopeDrafts("u1:ws1");
    expect(run1.store.getState().drafts).toEqual({});
    const ws1 = await coldStart("u1:ws1");
    expect(ws1.store.getState().drafts).toEqual({});
    const ws2 = await coldStart("u1:ws2");
    expect(ws2.store.getState().drafts["comment:i1"]?.text).toBe("ws2 draft");
  });

  it("deleting a workspace while its drafts load does not bring them back", async () => {
    const run1 = await coldStart("u1:ws1");
    run1.store.getState().patch("comment:i1", { text: "ws1 draft" });
    await settle();

    vi.resetModules();
    const store = (await import("./composer-drafts-store")).useComposerDraftsStore;
    const persistence = await import("../draft-persistence");
    const loading = store.getState().hydrate("u1:ws1");
    await persistence.clearScopeDrafts("u1:ws1");
    await loading;
    // a late upload callback of the deleted workspace
    store.getState().patchAttachments("comment:i1", () => [upload], "u1:ws1");
    await settle();

    expect(store.getState().drafts).toEqual({});
    const back = await coldStart("u1:ws1");
    expect(back.store.getState().drafts).toEqual({});
  });

  it("a reply and a mention chosen before the drafts load are kept next to the saved ones", async () => {
    const run1 = await coldStart();
    run1.store.getState().patch("comment:i1", {
      text: "hello",
      reply,
      mentions: [mention],
    });
    await settle();

    vi.resetModules();
    const store = (await import("./composer-drafts-store")).useComposerDraftsStore;
    const loading = store.getState().hydrate("u1:ws1");
    const newReply = { commentId: "c2", actorName: "Bo", preview: "hi" };
    const newMention = { type: "member" as const, id: "m1", name: "Mo" };
    store.getState().patch("comment:i1", { reply: newReply });
    store.getState().patch("comment:i1", { mentions: [newMention] });
    await loading;
    await settle();

    const expected = {
      text: "hello",
      reply: newReply,
      mentions: [mention, newMention],
      attachments: [],
    };
    expect(store.getState().drafts["comment:i1"]).toEqual(expected);
    const run2 = await coldStart();
    expect(run2.store.getState().drafts["comment:i1"]).toEqual(expected);
  });

  it("a mention picked and removed before the drafts load does not come back", async () => {
    const run1 = await coldStart();
    run1.store.getState().patch("comment:i1", { text: "hello", mentions: [mention] });
    await settle();

    vi.resetModules();
    const store = (await import("./composer-drafts-store")).useComposerDraftsStore;
    const loading = store.getState().hydrate("u1:ws1");
    const picked = { type: "member" as const, id: "m1", name: "Mo" };
    const kept = { type: "member" as const, id: "m2", name: "Al" };
    // The mention mirror pushes the whole live list on every toggle.
    store.getState().patch("comment:i1", { mentions: [picked, kept] });
    store.getState().patch("comment:i1", { mentions: [kept] });
    await loading;
    await settle();

    const expected = {
      text: "hello",
      reply: null,
      mentions: [mention, kept],
      attachments: [],
    };
    expect(store.getState().drafts["comment:i1"]).toEqual(expected);
    const run2 = await coldStart();
    expect(run2.store.getState().drafts["comment:i1"]).toEqual(expected);
  });

  describe("workspaces deleted while the device was offline", () => {
    const seed = async () => {
      for (const scope of ["u1:ws1", "u1:ws2", "u2:ws9"]) {
        const run = await coldStart(scope);
        run.store.getState().patch("comment:i1", { text: `${scope} draft` });
        await settle();
      }
    };

    it("drops the drafts of workspaces missing from the fetched list", async () => {
      await seed();
      const run = await coldStart("u1:ws1");
      await run.persistence.reconcileWorkspaceDrafts("u1", [{ id: "ws1" }]);

      expect((await coldStart("u1:ws2")).store.getState().drafts).toEqual({});
      const kept = await coldStart("u1:ws1");
      expect(kept.store.getState().drafts["comment:i1"]?.text).toBe("u1:ws1 draft");
      // Another account's drafts are not this list's business.
      const other = await coldStart("u2:ws9");
      expect(other.store.getState().drafts["comment:i1"]?.text).toBe("u2:ws9 draft");
    });

    it("also empties the memory of a dropped workspace", async () => {
      await seed();
      const run = await coldStart("u1:ws2");
      await run.persistence.reconcileWorkspaceDrafts("u1", [{ id: "ws1" }]);
      expect(run.store.getState().drafts).toEqual({});
    });

    it("a successful empty list drops every draft of the user", async () => {
      await seed();
      const run = await coldStart("u1:ws1");
      await run.persistence.reconcileWorkspaceDrafts("u1", []);

      expect(run.store.getState().drafts).toEqual({});
      expect((await coldStart("u1:ws1")).store.getState().drafts).toEqual({});
      expect((await coldStart("u1:ws2")).store.getState().drafts).toEqual({});
      const other = await coldStart("u2:ws9");
      expect(other.store.getState().drafts["comment:i1"]?.text).toBe("u2:ws9 draft");
    });

    it.each([
      ["a failed fetch", undefined],
      ["no list yet", null],
    ])("deletes nothing after %s", async (_name, list) => {
      await seed();
      const run = await coldStart("u1:ws1");
      await run.persistence.reconcileWorkspaceDrafts("u1", list);
      expect((await coldStart("u1:ws2")).store.getState().drafts["comment:i1"]?.text).toBe(
        "u1:ws2 draft",
      );
    });
  });
});
