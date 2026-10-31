// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { reRegisterThisDevice, resubscribeThisDevice } from "./push-sync";

afterEach(() => vi.unstubAllGlobals());

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

describe("reRegisterThisDevice", () => {
  it("re-sends an existing subscription so the server rebinds it to this session", async () => {
    stubBrowser({ existing: subscription() });
    const api = { registerPushSubscription: vi.fn(async () => undefined) };
    await expect(reRegisterThisDevice(api)).resolves.toBe("registered");
    expect(api.registerPushSubscription).toHaveBeenCalledWith({
      endpoint: "https://push.example/1",
      keys: { p256dh: "p", auth: "a" },
    });
  });

  it("does nothing without a subscription", async () => {
    stubBrowser({ existing: null });
    const api = { registerPushSubscription: vi.fn() };
    await expect(reRegisterThisDevice(api)).resolves.toBe("skipped");
    expect(api.registerPushSubscription).not.toHaveBeenCalled();
  });

  it("does nothing, and never prompts, unless permission is already granted", async () => {
    stubBrowser({ permission: "default", existing: subscription() });
    const api = { registerPushSubscription: vi.fn() };
    await expect(reRegisterThisDevice(api)).resolves.toBe("skipped");
    expect(api.registerPushSubscription).not.toHaveBeenCalled();
  });

  it("does nothing where push is unsupported", async () => {
    const api = { registerPushSubscription: vi.fn() };
    await expect(reRegisterThisDevice(api)).resolves.toBe("skipped");
    expect(api.registerPushSubscription).not.toHaveBeenCalled();
  });

  it("swallows a failed registration so a page load never breaks", async () => {
    stubBrowser({ existing: subscription() });
    const api = { registerPushSubscription: vi.fn().mockRejectedValue(new Error("offline")) };
    await expect(reRegisterThisDevice(api)).resolves.toBe("skipped");
  });
});

describe("resubscribeThisDevice", () => {
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
