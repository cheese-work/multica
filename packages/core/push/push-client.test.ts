// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  getPushSupport,
  registrationBody,
  subscribeThisDevice,
  unsubscribeThisDevice,
  urlBase64ToBytes,
} from "./push-client";

afterEach(() => vi.unstubAllGlobals());

const IPHONE_UA =
  "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1";
const CHROME_UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/130.0 Safari/537.36";

function stubBrowser(opts: {
  ua?: string;
  standalone?: boolean;
  push?: boolean;
  serviceWorker?: boolean;
  notification?: boolean;
}) {
  const win: Record<string, unknown> = {
    matchMedia: (q: string) => ({ matches: opts.standalone === true && q.includes("standalone") }),
  };
  if (opts.push !== false) win.PushManager = class {};
  if (opts.notification !== false) win.Notification = class {};
  const nav: Record<string, unknown> = { userAgent: opts.ua ?? CHROME_UA, maxTouchPoints: 0 };
  if (opts.serviceWorker !== false) nav.serviceWorker = {};
  if (opts.standalone) nav.standalone = true;
  vi.stubGlobal("window", win);
  vi.stubGlobal("navigator", nav);
}

describe("getPushSupport", () => {
  it("is unsupported without window (SSR)", () => {
    expect(getPushSupport()).toBe("unsupported");
  });

  it("is ok on desktop Chrome", () => {
    stubBrowser({});
    expect(getPushSupport()).toBe("ok");
  });

  it("is unsupported when PushManager is missing off iOS", () => {
    stubBrowser({ push: false });
    expect(getPushSupport()).toBe("unsupported");
  });

  it("asks iPhone Safari outside the Home Screen to add the app", () => {
    stubBrowser({ ua: IPHONE_UA, push: false });
    expect(getPushSupport()).toBe("needs-home-screen");
  });

  it("is ok in an iPhone Home Screen app", () => {
    stubBrowser({ ua: IPHONE_UA, standalone: true });
    expect(getPushSupport()).toBe("ok");
  });

  it("treats an iPad that reports as a Mac the same way", () => {
    stubBrowser({ ua: CHROME_UA.replace("X11; Linux x86_64", "Macintosh; Intel Mac OS X 10_15_7"), push: false });
    (navigator as unknown as { maxTouchPoints: number }).maxTouchPoints = 5;
    expect(getPushSupport()).toBe("needs-home-screen");
  });
});

describe("urlBase64ToBytes", () => {
  it("decodes unpadded base64url", () => {
    expect(Array.from(urlBase64ToBytes("-_8"))).toEqual([251, 255]);
  });
});

describe("registrationBody", () => {
  it("maps a subscription to the server contract", () => {
    const sub = { toJSON: () => ({ endpoint: "https://push.example/x", keys: { p256dh: "P", auth: "A" }, expirationTime: null }) };
    expect(registrationBody(sub as unknown as PushSubscription)).toEqual({
      endpoint: "https://push.example/x",
      keys: { p256dh: "P", auth: "A" },
    });
  });

  it("returns null when keys are missing", () => {
    const sub = { toJSON: () => ({ endpoint: "https://push.example/x" }) };
    expect(registrationBody(sub as unknown as PushSubscription)).toBeNull();
  });
});

function fakeRegistration(existing: PushSubscription | null) {
  const subscribe = vi.fn().mockResolvedValue({ toJSON: () => ({ endpoint: "e", keys: { p256dh: "p", auth: "a" } }) });
  return {
    subscribe,
    registration: {
      pushManager: { getSubscription: vi.fn().mockResolvedValue(existing), subscribe },
    } as unknown as ServiceWorkerRegistration,
  };
}

describe("subscribeThisDevice", () => {
  it("reuses an existing subscription for the same key without subscribing again", async () => {
    const existing = { toJSON: () => ({ endpoint: "e", keys: { p256dh: "p", auth: "a" } }), options: { applicationServerKey: urlBase64ToBytes("-_8").buffer } } as unknown as PushSubscription;
    const { registration, subscribe } = fakeRegistration(existing);
    const body = await subscribeThisDevice(registration, "-_8");
    expect(body).toEqual({ endpoint: "e", keys: { p256dh: "p", auth: "a" } });
    expect(subscribe).not.toHaveBeenCalled();
  });

  it("subscribes with userVisibleOnly and the deployment key", async () => {
    const { registration, subscribe } = fakeRegistration(null);
    await subscribeThisDevice(registration, "-_8");
    expect(subscribe).toHaveBeenCalledWith({
      userVisibleOnly: true,
      applicationServerKey: expect.any(Uint8Array),
    });
  });
});

describe("unsubscribeThisDevice", () => {
  function setup(opts: { deleteFails?: boolean; unsubscribeFails?: boolean; sub?: boolean }) {
    const unsubscribe = vi.fn(opts.unsubscribeFails ? () => Promise.reject(new Error("x")) : () => Promise.resolve(true));
    const sub = opts.sub === false ? null : { endpoint: "https://push.example/x", unsubscribe };
    const getRegistration = vi.fn().mockResolvedValue({ pushManager: { getSubscription: vi.fn().mockResolvedValue(sub) } });
    vi.stubGlobal("navigator", { serviceWorker: { getRegistration } });
    vi.stubGlobal("window", {});
    const deletePushSubscription = vi.fn(opts.deleteFails ? () => Promise.reject(new Error("net")) : () => Promise.resolve());
    return { unsubscribe, deletePushSubscription };
  }

  it("deletes on the server first, then unsubscribes", async () => {
    const { unsubscribe, deletePushSubscription } = setup({});
    const order: string[] = [];
    deletePushSubscription.mockImplementation(async () => void order.push("delete"));
    unsubscribe.mockImplementation(async () => (order.push("unsubscribe"), true));
    await unsubscribeThisDevice({ deletePushSubscription });
    expect(deletePushSubscription).toHaveBeenCalledWith("https://push.example/x");
    expect(order).toEqual(["delete", "unsubscribe"]);
  });

  it("still unsubscribes and never throws when the server delete fails", async () => {
    const { unsubscribe, deletePushSubscription } = setup({ deleteFails: true });
    await expect(unsubscribeThisDevice({ deletePushSubscription })).resolves.toBeUndefined();
    expect(unsubscribe).toHaveBeenCalled();
  });

  it("is a no-op without a subscription", async () => {
    const { deletePushSubscription } = setup({ sub: false });
    await unsubscribeThisDevice({ deletePushSubscription });
    expect(deletePushSubscription).not.toHaveBeenCalled();
  });

  it("does not throw when the browser has no service worker support", async () => {
    vi.stubGlobal("window", {});
    vi.stubGlobal("navigator", {});
    await expect(unsubscribeThisDevice({ deletePushSubscription: vi.fn() })).resolves.toBeUndefined();
  });
});
