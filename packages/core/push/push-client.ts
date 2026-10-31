// Browser side of Web Push (CHE-1798). Headless: it only touches `window`,
// `navigator` and the registration it is handed, so the views layer owns UI.

export type PushSupport = "ok" | "unsupported" | "needs-home-screen";

export interface PushRegistrationBody {
  endpoint: string;
  keys: { p256dh: string; auth: string };
}

function isIos(nav: Navigator): boolean {
  if (/iPhone|iPad|iPod/.test(nav.userAgent)) return true;
  // iPadOS Safari reports as a Mac but has a touch screen.
  return /Macintosh/.test(nav.userAgent) && nav.maxTouchPoints > 1;
}

function isHomeScreenApp(win: Window, nav: Navigator): boolean {
  return (
    (nav as { standalone?: boolean }).standalone === true ||
    win.matchMedia?.("(display-mode: standalone)").matches === true
  );
}

/**
 * Whether this browser can take push. iOS Safari outside a Home Screen app has
 * no `PushManager`; the fix there is "Add to Home Screen", not "unsupported".
 */
export function getPushSupport(): PushSupport {
  if (typeof window === "undefined" || typeof navigator === "undefined") return "unsupported";
  if (isIos(navigator) && !isHomeScreenApp(window, navigator)) return "needs-home-screen";
  const capable = "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;
  return capable ? "ok" : "unsupported";
}

export function urlBase64ToBytes(value: string): Uint8Array<ArrayBuffer> {
  const padded = value.replace(/-/g, "+").replace(/_/g, "/").padEnd(Math.ceil(value.length / 4) * 4, "=");
  const raw = atob(padded);
  const bytes = new Uint8Array(new ArrayBuffer(raw.length));
  for (let i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i);
  return bytes;
}

export function registrationBody(subscription: PushSubscription): PushRegistrationBody | null {
  const { endpoint, keys } = subscription.toJSON();
  if (!endpoint || !keys?.p256dh || !keys.auth) return null;
  return { endpoint, keys: { p256dh: keys.p256dh, auth: keys.auth } };
}

function sameKey(subscription: PushSubscription, key: Uint8Array): boolean {
  const current = subscription.options?.applicationServerKey;
  if (!current) return false;
  const a = new Uint8Array(current);
  return a.length === key.length && a.every((byte, i) => byte === key[i]);
}

/**
 * Subscribes this browser with the deployment's VAPID key and returns the body
 * to register. A subscription made with another key (the deployment rotated its
 * keys) is dropped first, because the push service would reject sends to it.
 */
export async function subscribeThisDevice(
  registration: ServiceWorkerRegistration,
  publicKey: string,
): Promise<PushRegistrationBody | null> {
  const key = urlBase64ToBytes(publicKey);
  const existing = await registration.pushManager.getSubscription();
  if (existing && sameKey(existing, key)) return registrationBody(existing);
  if (existing) await existing.unsubscribe();
  const created = await registration.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: key,
  });
  return registrationBody(created);
}

/** The registration for `/sw.js`, installing it on first use. */
export async function ensurePushRegistration(): Promise<ServiceWorkerRegistration> {
  await navigator.serviceWorker.register("/sw.js", { scope: "/" });
  return navigator.serviceWorker.ready;
}

export async function getThisDeviceSubscription(): Promise<PushSubscription | null> {
  if (typeof navigator === "undefined" || !("serviceWorker" in navigator)) return null;
  const registration = await navigator.serviceWorker.getRegistration("/");
  return registration ? registration.pushManager.getSubscription() : null;
}

/**
 * Logout and the device switch. Server delete first (it needs the session),
 * then drop the browser subscription. A failure never blocks the caller: the
 * worker also skips a subscription whose registering session has ended.
 */
export async function unsubscribeThisDevice(api: {
  deletePushSubscription: (endpoint: string) => Promise<void>;
}): Promise<void> {
  let subscription: PushSubscription | null = null;
  try {
    subscription = await getThisDeviceSubscription();
  } catch {
    return;
  }
  if (!subscription) return;
  try {
    await api.deletePushSubscription(subscription.endpoint);
  } catch {
    // Best effort, see above.
  }
  try {
    await subscription.unsubscribe();
  } catch {
    // The browser may already have dropped it.
  }
}
