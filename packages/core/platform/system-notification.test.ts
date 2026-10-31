import { afterEach, describe, expect, it, vi } from "vitest";
import {
  getWebNotificationPermission,
  isWebNotificationSupported,
  registerSystemNotificationClickHandler,
  requestWebNotificationPermission,
  showWebNotification,
  type SystemNotificationPayload,
} from "./system-notification";

// The core test environment is Node — `window`/`Notification` don't exist.
// These tests install a minimal `window.Notification` stub on `globalThis`
// (the same surface `getNotificationCtor` reads) to exercise the browser path.

type Created = { title: string; options?: NotificationOptions; instance: FakeNotification };

class FakeNotification {
  static permission: NotificationPermission = "granted";
  static requestPermission = vi.fn(async () => FakeNotification.permission);
  onclick: (() => void) | null = null;
  close = vi.fn();
  constructor(
    public title: string,
    public options?: NotificationOptions,
  ) {
    created.push({ title, options, instance: this });
  }
}

let created: Created[] = [];

function installWindow(
  ctor: unknown,
  focus: () => void = () => {},
): void {
  (globalThis as Record<string, unknown>).window = { Notification: ctor, focus };
}

function payload(
  overrides: Partial<SystemNotificationPayload> = {},
): SystemNotificationPayload {
  return {
    slug: "workspace-a",
    itemId: "item-1",
    issueKey: "issue-1",
    title: "Mentioned you",
    body: "in a comment",
    ...overrides,
  };
}

afterEach(() => {
  created = [];
  FakeNotification.permission = "granted";
  FakeNotification.requestPermission.mockClear();
  registerSystemNotificationClickHandler(null);
  delete (globalThis as Record<string, unknown>).window;
  vi.unstubAllGlobals();
});

describe("isWebNotificationSupported / getWebNotificationPermission", () => {
  it("reports unsupported when there is no window", () => {
    expect(isWebNotificationSupported()).toBe(false);
    expect(getWebNotificationPermission()).toBe("unsupported");
  });

  it("reflects the browser's current permission when supported", () => {
    installWindow(FakeNotification);
    FakeNotification.permission = "denied";
    expect(isWebNotificationSupported()).toBe(true);
    expect(getWebNotificationPermission()).toBe("denied");
  });
});

describe("requestWebNotificationPermission", () => {
  it("returns 'unsupported' without the API", async () => {
    await expect(requestWebNotificationPermission()).resolves.toBe("unsupported");
  });

  it("prompts only when permission is 'default'", async () => {
    installWindow(FakeNotification);
    FakeNotification.permission = "default";
    FakeNotification.requestPermission.mockResolvedValueOnce("granted");

    await expect(requestWebNotificationPermission()).resolves.toBe("granted");
    expect(FakeNotification.requestPermission).toHaveBeenCalledTimes(1);
  });

  it("does not re-prompt once already decided", async () => {
    installWindow(FakeNotification);
    FakeNotification.permission = "denied";

    await expect(requestWebNotificationPermission()).resolves.toBe("denied");
    expect(FakeNotification.requestPermission).not.toHaveBeenCalled();
  });
});

describe("showWebNotification", () => {
  it("does nothing when the API is unavailable", () => {
    expect(() => showWebNotification(payload())).not.toThrow();
    expect(created).toHaveLength(0);
  });

  it("does nothing unless permission is granted", () => {
    installWindow(FakeNotification);
    FakeNotification.permission = "default";
    showWebNotification(payload());
    expect(created).toHaveLength(0);
  });

  it("shows a banner with body + a dedup tag when granted", () => {
    installWindow(FakeNotification);
    showWebNotification(payload());
    expect(created).toHaveLength(1);
    expect(created[0]?.title).toBe("Mentioned you");
    expect(created[0]?.options).toMatchObject({ body: "in a comment", tag: "item-1" });
  });

  it("routes a click to the registered handler and closes the banner", () => {
    const focus = vi.fn();
    installWindow(FakeNotification, focus);
    const onClick = vi.fn();
    registerSystemNotificationClickHandler(onClick);

    showWebNotification(payload());
    created[0]?.instance.onclick?.();

    expect(focus).toHaveBeenCalledTimes(1);
    expect(created[0]?.instance.close).toHaveBeenCalledTimes(1);
    expect(onClick).toHaveBeenCalledWith(payload());
  });

  it("swallows constructors that throw (e.g. service-worker-only engines)", () => {
    class ThrowingNotification {
      static permission: NotificationPermission = "granted";
      constructor() {
        throw new Error("requires a ServiceWorkerRegistration");
      }
    }
    installWindow(ThrowingNotification);
    expect(() => showWebNotification(payload())).not.toThrow();
  });
});

describe("showWebNotification with a service worker registration", () => {
  const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

  function installRegistration(showNotification: (title: string, options?: unknown) => Promise<void>) {
    const getRegistration = vi.fn(async () => ({ showNotification }));
    vi.stubGlobal("navigator", { serviceWorker: { getRegistration } });
    return getRegistration;
  }

  it("shows through the registration so a push for the same record replaces it (one banner)", async () => {
    installWindow(FakeNotification);
    const showNotification = vi.fn(async () => undefined);
    installRegistration(showNotification);

    showWebNotification(payload());
    await flush();

    expect(created).toHaveLength(0);
    // The worker's push handler uses this registration with tag = record id.
    expect(showNotification).toHaveBeenCalledWith("Mentioned you", {
      body: "in a comment",
      tag: "item-1",
      data: { url: "/workspace-a/inbox?issue=issue-1" },
    });
  });

  it("points a DM banner at its chat session", async () => {
    installWindow(FakeNotification);
    const showNotification = vi.fn(async () => undefined);
    installRegistration(showNotification);

    showWebNotification(payload({ chatSessionId: "sess-9" }));
    await flush();

    expect(showNotification).toHaveBeenCalledWith(
      "Mentioned you",
      expect.objectContaining({ data: { url: "/workspace-a/chat?session=sess-9" } }),
    );
  });

  it("falls back to an in-page banner when no worker is registered", async () => {
    installWindow(FakeNotification);
    vi.stubGlobal("navigator", { serviceWorker: { getRegistration: vi.fn(async () => undefined) } });

    showWebNotification(payload());
    await flush();

    expect(created).toHaveLength(1);
  });

  it("falls back to an in-page banner when the registration refuses", async () => {
    installWindow(FakeNotification);
    installRegistration(vi.fn(async () => Promise.reject(new Error("no"))));

    showWebNotification(payload());
    await flush();

    expect(created).toHaveLength(1);
  });

  it("shows nothing without a workspace slug to route to", async () => {
    installWindow(FakeNotification);
    const showNotification = vi.fn(async () => undefined);
    installRegistration(showNotification);

    showWebNotification(payload({ slug: "" }));
    await flush();

    expect(showNotification).not.toHaveBeenCalled();
    expect(created).toHaveLength(1);
  });
});
