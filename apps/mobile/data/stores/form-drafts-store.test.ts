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
  return {
    ...(await import("./form-drafts-store")),
    ...(await import("../draft-persistence")),
  };
}

const settle = () => vi.advanceTimersByTimeAsync(1000);

describe("form drafts survive an app cold start", () => {
  beforeEach(() => {
    disk.clear();
    vi.useFakeTimers();
  });
  afterEach(() => vi.useRealTimers());

  it.each([
    ["issue edit", (m: Awaited<ReturnType<typeof coldStart>>) => m.issueEditKey("i1"), { title: "T", description: "half-typed" }],
    ["project edit", (m: Awaited<ReturnType<typeof coldStart>>) => m.projectEditKey("p1"), { title: "T", description: "d", icon: "📦" }],
    ["new issue", (m: Awaited<ReturnType<typeof coldStart>>) => m.NEW_ISSUE_KEY, { title: "T", description: "d", status: "todo" }],
    ["new project", (m: Awaited<ReturnType<typeof coldStart>>) => m.NEW_PROJECT_KEY, { title: "T", description: "d", icon: "" }],
  ])("restores the %s draft", async (_name, keyOf, value) => {
    const run1 = await coldStart();
    await run1.useFormDraftsStore.getState().hydrate("u:ws1");
    run1.useFormDraftsStore.getState().setDraft(keyOf(run1), value);
    await settle();

    const run2 = await coldStart();
    await run2.useFormDraftsStore.getState().hydrate("u:ws1");
    expect(run2.readFormDraft(keyOf(run2))).toEqual(value);
  });

  it("keeps the draft after a failed save and drops it after a successful one", async () => {
    const run1 = await coldStart();
    const s = run1.useFormDraftsStore.getState();
    await s.hydrate("u:ws1");
    const key = run1.issueEditKey("i1");
    s.setDraft(key, { title: "T", description: "d" });
    await settle();
    // failed save: the screen does not clear
    const run2 = await coldStart();
    await run2.useFormDraftsStore.getState().hydrate("u:ws1");
    expect(run2.readFormDraft(key)).toBeDefined();
    // successful save: the screen clears
    run2.useFormDraftsStore.getState().clearDraft(key);
    await settle();
    const run3 = await coldStart();
    await run3.useFormDraftsStore.getState().hydrate("u:ws1");
    expect(run3.readFormDraft(key)).toBeUndefined();
  });

  it("scopes drafts per workspace and per entity", async () => {
    const run1 = await coldStart();
    const s = run1.useFormDraftsStore.getState();
    await s.hydrate("u:ws1");
    s.setDraft(run1.issueEditKey("i1"), { title: "one", description: "" });
    await settle();

    const run2 = await coldStart();
    await run2.useFormDraftsStore.getState().hydrate("u:ws2");
    expect(run2.readFormDraft(run2.issueEditKey("i1"))).toBeUndefined();
    await run2.useFormDraftsStore.getState().hydrate("u:ws1");
    expect(run2.readFormDraft(run2.issueEditKey("i1"))).toBeDefined();
    expect(run2.readFormDraft(run2.issueEditKey("i2"))).toBeUndefined();
  });

  it("is not readable before the disk has loaded", async () => {
    const run1 = await coldStart();
    expect(run1.readFormDraft(run1.NEW_ISSUE_KEY)).toBeUndefined();
    expect(run1.useFormDraftsStore.getState().ready).toBe(false);
  });

  it("is wiped from memory and disk on logout", async () => {
    const run1 = await coldStart();
    const s = run1.useFormDraftsStore.getState();
    await s.hydrate("u:ws1");
    s.setDraft(run1.NEW_ISSUE_KEY, { title: "x", description: "" });
    await settle();
    await run1.clearAllDrafts();

    expect(run1.useFormDraftsStore.getState().drafts).toEqual({});
    const run2 = await coldStart();
    await run2.useFormDraftsStore.getState().hydrate("u:ws1");
    expect(run2.readFormDraft(run2.NEW_ISSUE_KEY)).toBeUndefined();
  });

  it("drops a draft older than 30 days", async () => {
    const run1 = await coldStart();
    await run1.useFormDraftsStore.getState().hydrate("u:ws1");
    run1.useFormDraftsStore.getState().setDraft(run1.NEW_ISSUE_KEY, { title: "old", description: "" });
    await settle();
    vi.setSystemTime(Date.now() + run1.DRAFT_TTL_MS + 60_000);

    const run2 = await coldStart();
    await run2.useFormDraftsStore.getState().hydrate("u:ws1");
    expect(run2.readFormDraft(run2.NEW_ISSUE_KEY)).toBeUndefined();
  });

  it("does not resurrect a draft cleared while the disk was loading", async () => {
    const run1 = await coldStart();
    await run1.useFormDraftsStore.getState().hydrate("u:ws1");
    run1.useFormDraftsStore.getState().setDraft(run1.NEW_ISSUE_KEY, { title: "x", description: "" });
    await settle();

    const run2 = await coldStart();
    const hydrating = run2.useFormDraftsStore.getState().hydrate("u:ws1");
    run2.useFormDraftsStore.getState().clearDraft(run2.NEW_ISSUE_KEY);
    await hydrating;
    expect(run2.readFormDraft(run2.NEW_ISSUE_KEY)).toBeUndefined();
  });
});
