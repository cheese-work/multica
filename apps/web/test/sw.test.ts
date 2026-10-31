// @vitest-environment node
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { beforeEach, describe, expect, it, vi } from "vitest";

type Listener = (event: Record<string, unknown>) => void;

const source = readFileSync(join(__dirname, "../public/sw.js"), "utf8");

function loadWorker() {
  const listeners: Record<string, Listener> = {};
  const shown: { title: string; options: Record<string, unknown> }[] = [];
  const windows: {
    url: string;
    focus: ReturnType<typeof vi.fn>;
    navigate: ReturnType<typeof vi.fn>;
    postMessage: ReturnType<typeof vi.fn>;
  }[] = [];
  const openWindow = vi.fn();
  const subscribe = vi.fn(async () => ({ endpoint: "https://push.example/new" }));
  const scope = {
    location: { origin: "https://app.example" },
    addEventListener: (type: string, fn: Listener) => {
      listeners[type] = fn;
    },
    registration: {
      showNotification: async (title: string, options: Record<string, unknown>) => {
        shown.push({ title, options });
      },
      pushManager: { subscribe },
    },
    clients: {
      matchAll: async () => windows,
      openWindow,
    },
  };
  new Function("self", source)(scope);
  const dispatch = async (type: string, event: Record<string, unknown>) => {
    const waits: Promise<unknown>[] = [];
    listeners[type]?.({ ...event, waitUntil: (p: Promise<unknown>) => waits.push(p) });
    await Promise.all(waits);
  };
  return { dispatch, shown, windows, openWindow, subscribe, listeners };
}

const pushEvent = (payload: unknown) => ({
  data: { json: () => payload, text: () => JSON.stringify(payload) },
});

const fakeWindow = (url: string) => ({
  url,
  focus: vi.fn(async () => undefined),
  navigate: vi.fn(async () => undefined),
  postMessage: vi.fn(),
});

describe("service worker push", () => {
  let worker: ReturnType<typeof loadWorker>;
  beforeEach(() => {
    worker = loadWorker();
  });

  it("shows the sender and the fixed label, tagged with the record id", async () => {
    await worker.dispatch(
      "push",
      pushEvent({
        record_id: "r1",
        kind: "dm_reply",
        actor_name: "Atlas",
        label: "New reply",
        reply_count: 1,
        url: "/acme/chat?session=s1",
      }),
    );
    expect(worker.shown).toEqual([
      {
        title: "Atlas",
        options: expect.objectContaining({
          body: "New reply",
          tag: "r1",
          data: { url: "/acme/chat?session=s1" },
        }),
      },
    ]);
  });

  it("counts coalesced DM replies", async () => {
    await worker.dispatch(
      "push",
      pushEvent({ record_id: "r1", kind: "dm_reply", actor_name: "Atlas", label: "New reply", reply_count: 4, url: "/a/chat?session=s" }),
    );
    expect(worker.shown[0]?.options.body).toBe("4 new replies");
  });

  it("shows the issue label for an issue record", async () => {
    await worker.dispatch(
      "push",
      pushEvent({ record_id: "r2", kind: "mentioned", actor_name: "Ada", label: "MUL-1 Fix login", reply_count: 0, url: "/acme/issues/MUL-1" }),
    );
    expect(worker.shown[0]).toMatchObject({ title: "Ada", options: { body: "MUL-1 Fix login", tag: "r2" } });
  });

  // iOS drops the subscription after a push that shows nothing.
  it("still shows a notification when the payload is empty or malformed", async () => {
    await worker.dispatch("push", { data: null });
    await worker.dispatch("push", { data: { json: () => { throw new Error("bad"); }, text: () => "nope" } });
    expect(worker.shown.map((n) => n.title)).toEqual(["Multica", "Multica"]);
    expect(worker.shown[0]?.options.data).toEqual({ url: "/" });
  });

  it("never turns an off-origin url into a navigation target", async () => {
    await worker.dispatch(
      "push",
      pushEvent({ record_id: "r3", actor_name: "X", label: "Notification", url: "https://evil.example/phish" }),
    );
    expect(worker.shown[0]?.options.data).toEqual({ url: "/" });
  });
});

describe("service worker notificationclick", () => {
  const click = (url: string) => ({
    notification: { close: vi.fn(), data: { url } },
  });

  it("focuses an open tab and sends it to the url", async () => {
    const worker = loadWorker();
    const tab = fakeWindow("https://app.example/acme/inbox");
    worker.windows.push(tab);
    await worker.dispatch("notificationclick", click("/acme/chat?session=s1"));
    expect(tab.focus).toHaveBeenCalled();
    expect(tab.navigate).toHaveBeenCalledWith("https://app.example/acme/chat?session=s1");
    expect(worker.openWindow).not.toHaveBeenCalled();
  });

  it("does not reload a tab that is already on the url", async () => {
    const worker = loadWorker();
    const tab = fakeWindow("https://app.example/acme/chat?session=s1");
    worker.windows.push(tab);
    await worker.dispatch("notificationclick", click("/acme/chat?session=s1"));
    expect(tab.focus).toHaveBeenCalled();
    expect(tab.navigate).not.toHaveBeenCalled();
  });

  it("opens a window when no tab is open", async () => {
    const worker = loadWorker();
    await worker.dispatch("notificationclick", click("/acme/chat?session=s1"));
    expect(worker.openWindow).toHaveBeenCalledWith("https://app.example/acme/chat?session=s1");
  });

  it("opens a window when the open tab cannot navigate", async () => {
    const worker = loadWorker();
    const tab = fakeWindow("https://app.example/x");
    tab.navigate.mockRejectedValue(new Error("not allowed"));
    worker.windows.push(tab);
    await worker.dispatch("notificationclick", click("/acme/inbox"));
    expect(worker.openWindow).toHaveBeenCalledWith("https://app.example/acme/inbox");
  });

  it("closes the notification", async () => {
    const worker = loadWorker();
    const event = click("/acme/inbox");
    await worker.dispatch("notificationclick", event);
    expect(event.notification.close).toHaveBeenCalled();
  });
});

describe("service worker pushsubscriptionchange", () => {
  it("re-subscribes with the old key and tells open tabs to re-register", async () => {
    const worker = loadWorker();
    const tab = fakeWindow("https://app.example/acme");
    worker.windows.push(tab);
    const key = new Uint8Array([1, 2, 3]).buffer;
    await worker.dispatch("pushsubscriptionchange", {
      oldSubscription: { options: { applicationServerKey: key } },
    });
    expect(worker.subscribe).toHaveBeenCalledWith({ userVisibleOnly: true, applicationServerKey: key });
    expect(tab.postMessage).toHaveBeenCalledWith({ type: "push-subscription-changed" });
  });

  it("renews quietly with no tab open; the app registers the new endpoint on its next load", async () => {
    const worker = loadWorker();
    const key = new Uint8Array([1, 2, 3]).buffer;
    await expect(
      worker.dispatch("pushsubscriptionchange", { oldSubscription: { options: { applicationServerKey: key } } }),
    ).resolves.toBeUndefined();
    expect(worker.subscribe).toHaveBeenCalledWith({ userVisibleOnly: true, applicationServerKey: key });
  });

  it("still tells open tabs when renewal fails, so one of them can recover it", async () => {
    const worker = loadWorker();
    const tab = fakeWindow("https://app.example/acme");
    worker.windows.push(tab);
    worker.subscribe.mockRejectedValue(new Error("denied"));
    await worker.dispatch("pushsubscriptionchange", {
      oldSubscription: { options: { applicationServerKey: new Uint8Array([1]).buffer } },
    });
    expect(tab.postMessage).toHaveBeenCalledWith({ type: "push-subscription-changed" });
  });

  it("does not throw when it cannot re-subscribe", async () => {
    const worker = loadWorker();
    worker.subscribe.mockRejectedValue(new Error("denied"));
    await expect(
      worker.dispatch("pushsubscriptionchange", { oldSubscription: null }),
    ).resolves.toBeUndefined();
  });
});
