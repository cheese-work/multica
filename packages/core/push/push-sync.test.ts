// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { setPushIntent } from "./push-intent";
import { reRegisterThisDevice, resubscribeThisDevice } from "./push-sync";

afterEach(() => vi.unstubAllGlobals());

function stubStorage(initial?: "on" | "off") {
  const store = new Map<string, string>();
  vi.stubGlobal("localStorage", {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, v),
  });
  if (initial) setPushIntent(initial);
}

const KEY = "BAEA"; // any base64url; subscribe is stubbed

function subscription(endpoint = "https://push.example/1") {
  return {
    endpoint,
    options: { applicationServerKey: new Uint8Array([4, 1, 0]).buffer },
    toJSON: () => ({ endpoint, keys: { p256dh: "p", auth: "a" } }),
    unsubscribe: vi.fn(async () => true),
  };
}

function stubBrowser(opts: { permission?: string; existing?: ReturnType<typeof subscription> | null }) {
  const pushManager = {
    getSubscription: vi.fn(async () => opts.existing ?? null),
    subscribe: vi.fn(async () => subscription("https://push.example/new")),
  };
  const registration = { pushManager };
  vi.stubGlobal("window", {
    PushManager: class {},
    Notification: class {},
    matchMedia: () => ({ matches: false }),
  });
  (window.Notification as unknown as { permission: string }).permission = opts.permission ?? "granted";
  vi.stubGlobal("navigator", {
    userAgent: "Chrome",
    maxTouchPoints: 0,
    serviceWorker: {
      register: vi.fn(async () => registration),
      ready: Promise.resolve(registration),
      getRegistration: vi.fn(async () => (opts.existing === undefined ? undefined : registration)),
    },
  });
  return pushManager;
}

const config = () => vi.fn(async () => ({ enabled: true, public_key: KEY }));

describe("reRegisterThisDevice", () => {
  it("respects an opt-out: a subscription the browser failed to drop is not re-registered", async () => {
    stubStorage("off");
    stubBrowser({ existing: subscription() });
    const api = { registerPushSubscription: vi.fn(), getPushConfig: config() };
    await expect(reRegisterThisDevice(api)).resolves.toBe("skipped");
    expect(api.registerPushSubscription).not.toHaveBeenCalled();
  });

  it("respects an opt-out even when storage refused to record it", async () => {
    vi.resetModules();
    const intent = await import("./push-intent");
    const sync = await import("./push-sync");
    vi.stubGlobal("localStorage", {
      getItem: () => "on",
      setItem: () => {
        throw new Error("QuotaExceededError");
      },
    });
    stubBrowser({ existing: subscription() });
    intent.setPushIntent("off");
    const api = { registerPushSubscription: vi.fn(), getPushConfig: config() };
    await expect(sync.reRegisterThisDevice(api)).resolves.toBe("skipped");
    expect(api.registerPushSubscription).not.toHaveBeenCalled();
  });

  it("recovers a subscription the browser dropped while push is wanted", async () => {
    stubStorage("on");
    const pushManager = stubBrowser({ existing: null });
    const api = { registerPushSubscription: vi.fn(async () => undefined), getPushConfig: config() };
    await expect(reRegisterThisDevice(api)).resolves.toBe("registered");
    expect(pushManager.subscribe).toHaveBeenCalled();
    expect(api.registerPushSubscription).toHaveBeenCalledWith({
      endpoint: "https://push.example/new",
      keys: { p256dh: "p", auth: "a" },
    });
  });

  it("does not resubscribe when this device never opted in", async () => {
    stubStorage();
    const pushManager = stubBrowser({ existing: null });
    const api = { registerPushSubscription: vi.fn(), getPushConfig: config() };
    await expect(reRegisterThisDevice(api)).resolves.toBe("skipped");
    expect(pushManager.subscribe).not.toHaveBeenCalled();
  });

  it("registers the renewed endpoint when a closed-tab renewal replaced it", async () => {
    stubStorage("on");
    stubBrowser({ existing: subscription("https://push.example/renewed") });
    const api = { registerPushSubscription: vi.fn(async () => undefined), getPushConfig: config() };
    await expect(reRegisterThisDevice(api)).resolves.toBe("registered");
    expect(api.registerPushSubscription).toHaveBeenCalledWith(
      expect.objectContaining({ endpoint: "https://push.example/renewed" }),
    );
  });

  it("re-sends an existing subscription so the server rebinds it to this session", async () => {
    stubBrowser({ existing: subscription() });
    const api = { registerPushSubscription: vi.fn(async () => undefined), getPushConfig: config() };
    await expect(reRegisterThisDevice(api)).resolves.toBe("registered");
    expect(api.registerPushSubscription).toHaveBeenCalledWith({
      endpoint: "https://push.example/1",
      keys: { p256dh: "p", auth: "a" },
    });
  });

  it("does nothing without a subscription", async () => {
    stubBrowser({ existing: null });
    const api = { registerPushSubscription: vi.fn(), getPushConfig: config() };
    await expect(reRegisterThisDevice(api)).resolves.toBe("skipped");
    expect(api.registerPushSubscription).not.toHaveBeenCalled();
  });

  it("does nothing, and never prompts, unless permission is already granted", async () => {
    stubBrowser({ permission: "default", existing: subscription() });
    const api = { registerPushSubscription: vi.fn(), getPushConfig: config() };
    await expect(reRegisterThisDevice(api)).resolves.toBe("skipped");
    expect(api.registerPushSubscription).not.toHaveBeenCalled();
  });

  it("does nothing where push is unsupported", async () => {
    const api = { registerPushSubscription: vi.fn(), getPushConfig: config() };
    await expect(reRegisterThisDevice(api)).resolves.toBe("skipped");
    expect(api.registerPushSubscription).not.toHaveBeenCalled();
  });

  it("swallows a failed registration so a page load never breaks", async () => {
    stubBrowser({ existing: subscription() });
    const api = { registerPushSubscription: vi.fn().mockRejectedValue(new Error("offline")), getPushConfig: config() };
    await expect(reRegisterThisDevice(api)).resolves.toBe("skipped");
  });
});

describe("resubscribeThisDevice", () => {
  it("skips after an opt-out", async () => {
    stubStorage("off");
    stubBrowser({ existing: null });
    const api = { getPushConfig: config(), registerPushSubscription: vi.fn() };
    await expect(resubscribeThisDevice(api)).resolves.toBe("skipped");
    expect(api.registerPushSubscription).not.toHaveBeenCalled();
  });

  it("subscribes with the deployment key and registers the result", async () => {
    const pushManager = stubBrowser({ existing: null });
    const api = {
      getPushConfig: vi.fn(async () => ({ enabled: true, public_key: KEY })),
      registerPushSubscription: vi.fn(async () => undefined),
    };
    await expect(resubscribeThisDevice(api)).resolves.toBe("registered");
    expect(pushManager.subscribe).toHaveBeenCalledWith(expect.objectContaining({ userVisibleOnly: true }));
    expect(api.registerPushSubscription).toHaveBeenCalledWith({
      endpoint: "https://push.example/new",
      keys: { p256dh: "p", auth: "a" },
    });
  });

  it("skips when push is not configured on the server", async () => {
    stubBrowser({ existing: null });
    const api = {
      getPushConfig: vi.fn(async () => ({ enabled: false, public_key: "" })),
      registerPushSubscription: vi.fn(),
    };
    await expect(resubscribeThisDevice(api)).resolves.toBe("skipped");
    expect(api.registerPushSubscription).not.toHaveBeenCalled();
  });

  it("skips when permission was revoked", async () => {
    stubBrowser({ permission: "denied", existing: null });
    const api = { getPushConfig: vi.fn(), registerPushSubscription: vi.fn() };
    await expect(resubscribeThisDevice(api)).resolves.toBe("skipped");
    expect(api.getPushConfig).not.toHaveBeenCalled();
  });
});
