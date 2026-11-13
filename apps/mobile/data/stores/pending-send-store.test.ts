import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// Same disk across cold starts, like the real AsyncStorage.
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

const apiMock = vi.hoisted(() => ({ sendChatMessage: vi.fn() }));
vi.mock("@/data/api", () => ({ api: apiMock }));

class HttpError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
  }
}

async function coldStart() {
  vi.resetModules();
  const store = await import("./pending-send-store");
  const persistence = await import("../draft-persistence");
  return { ...store, ...persistence };
}

const OK = { message_id: "m", task_id: "t", created_at: "" };

describe("pending send keeps one client_message_id (A22)", () => {
  beforeEach(() => {
    disk.clear();
    apiMock.sendChatMessage.mockReset();
    vi.useFakeTimers();
  });
  afterEach(() => vi.useRealTimers());

  it("classifies failures: 4xx except 408 is a rejection, the rest is unknown", async () => {
    const { classifySendFailure } = await coldStart();
    expect(classifySendFailure(new HttpError("x", 400))).toBe("rejected");
    expect(classifySendFailure(new HttpError("x", 409))).toBe("rejected");
    expect(classifySendFailure(new HttpError("x", 408))).toBe("unknown");
    expect(classifySendFailure(new HttpError("x", 503))).toBe("unknown");
    expect(classifySendFailure(new Error("Network request failed"))).toBe("unknown");
  });

  it("retry after a timeout reuses the id", async () => {
    const m = await coldStart();
    await m.usePendingSendStore.getState().hydrate("ws1");
    apiMock.sendChatMessage
      .mockRejectedValueOnce(new Error("timeout"))
      .mockResolvedValueOnce(OK);

    await expect(m.sendWithPendingId("s1", "hello")).rejects.toThrow("timeout");
    await m.sendWithPendingId("s1", "hello");

    const ids = apiMock.sendChatMessage.mock.calls.map((c) => c[2].clientMessageId);
    expect(ids[0]).toBeTruthy();
    expect(ids[1]).toBe(ids[0]);
  });

  it("retry after a 5xx reuses the id; edited text gets a new id", async () => {
    const m = await coldStart();
    await m.usePendingSendStore.getState().hydrate("ws1");
    apiMock.sendChatMessage.mockRejectedValue(new HttpError("boom", 503));
    await expect(m.sendWithPendingId("s1", "hello")).rejects.toThrow();
    await expect(m.sendWithPendingId("s1", "hello")).rejects.toThrow();
    await expect(m.sendWithPendingId("s1", "hello edited")).rejects.toThrow();
    const ids = apiMock.sendChatMessage.mock.calls.map((c) => c[2].clientMessageId);
    expect(ids[1]).toBe(ids[0]);
    expect(ids[2]).not.toBe(ids[0]);
  });

  it("a 4xx rejection forgets the id: the next send mints a new one", async () => {
    const m = await coldStart();
    await m.usePendingSendStore.getState().hydrate("ws1");
    apiMock.sendChatMessage
      .mockRejectedValueOnce(new HttpError("bad", 400))
      .mockResolvedValueOnce(OK);
    await expect(m.sendWithPendingId("s1", "hello")).rejects.toThrow();
    await m.sendWithPendingId("s1", "hello");
    const ids = apiMock.sendChatMessage.mock.calls.map((c) => c[2].clientMessageId);
    expect(ids[1]).not.toBe(ids[0]);
  });

  it("a delivered send forgets the id", async () => {
    const m = await coldStart();
    await m.usePendingSendStore.getState().hydrate("ws1");
    apiMock.sendChatMessage.mockResolvedValue(OK);
    await m.sendWithPendingId("s1", "hello");
    await m.sendWithPendingId("s1", "hello");
    const ids = apiMock.sendChatMessage.mock.calls.map((c) => c[2].clientMessageId);
    expect(ids[1]).not.toBe(ids[0]);
    expect(m.usePendingSendStore.getState().bySession).toEqual({});
  });

  it("an unknown-delivery id survives an app restart", async () => {
    const run1 = await coldStart();
    await run1.usePendingSendStore.getState().hydrate("ws1");
    apiMock.sendChatMessage.mockRejectedValueOnce(new Error("offline"));
    await expect(run1.sendWithPendingId("s1", "hello")).rejects.toThrow();
    await vi.advanceTimersByTimeAsync(1000);

    const run2 = await coldStart();
    await run2.usePendingSendStore.getState().hydrate("ws1");
    apiMock.sendChatMessage.mockResolvedValueOnce(OK);
    await run2.sendWithPendingId("s1", "hello");

    const ids = apiMock.sendChatMessage.mock.calls.map((c) => c[2].clientMessageId);
    expect(ids[1]).toBe(ids[0]);
  });

  it("passes attachment ids and ask ids through", async () => {
    const m = await coldStart();
    await m.usePendingSendStore.getState().hydrate("ws1");
    apiMock.sendChatMessage.mockResolvedValue(OK);
    await m.sendWithPendingId("s1", "hi", { attachmentIds: ["a1"], askIds: ["k1"] });
    expect(apiMock.sendChatMessage).toHaveBeenCalledWith("s1", "hi", {
      attachmentIds: ["a1"],
      clientMessageId: expect.any(String),
      askIds: ["k1"],
    });
  });
});
