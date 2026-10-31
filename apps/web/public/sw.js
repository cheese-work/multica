/* global self, URL */
// Web Push worker (CHE-1798). Payloads are content-private: sender, a fixed
// label and a url, never message text. Plain JS because Next serves public/
// as is. A renewal (pushsubscriptionchange) is registered by the worker itself,
// so it works with every tab closed: a same-origin POST that carries the CSRF
// value read through cookieStore. Only while the device positively wants push:
// the worker reads the choice the page saved (see pushWanted) and, if it is off
// or unreadable, renews and registers nothing. Where registering is not
// possible (no cookieStore, no CSRF cookie, a refused or failed request) open
// tabs are told, and the app reconciles on its next load: it registers the
// subscription it holds, or subscribes again if the user still wants push.

const FALLBACK_URL = "/";

function targetUrl(value) {
  if (typeof value !== "string") return FALLBACK_URL;
  try {
    const url = new URL(value, self.location.origin);
    return url.origin === self.location.origin ? url.pathname + url.search + url.hash : FALLBACK_URL;
  } catch {
    return FALLBACK_URL;
  }
}

function readPayload(event) {
  try {
    return event.data ? event.data.json() || {} : {};
  } catch {
    return {};
  }
}

function bodyOf(payload) {
  const count = Number(payload.reply_count) || 0;
  if (payload.kind === "dm_reply" && count > 1) return count + " new replies";
  return payload.label || "Notification";
}

self.addEventListener("push", (event) => {
  const payload = readPayload(event);
  // Always show something: iOS revokes a subscription after a silent push.
  event.waitUntil(
    self.registration.showNotification(payload.actor_name || "Multica", {
      body: bodyOf(payload),
      // Same tag as the in-tab banner, so one record is one banner.
      tag: payload.record_id || undefined,
      data: { url: targetUrl(payload.url) },
    }),
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = new URL(targetUrl(event.notification.data && event.notification.data.url), self.location.origin).href;
  event.waitUntil(
    (async () => {
      const tabs = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
      const tab = tabs.find((t) => t.url.startsWith(self.location.origin));
      if (tab) {
        try {
          await tab.focus();
          if (tab.url !== url) await tab.navigate(url);
          return;
        } catch {
          // Fall through to a new window.
        }
      }
      await self.clients.openWindow(url);
    })(),
  );
});

// The device's push choice, saved by the page (packages/core/push/push-intent.ts;
// keep these names in step with it). Fail closed: wanted only when IndexedDB
// positively says "on" and the opt-out cookie is absent; any error means no.
const INTENT_DB = "multica-push";
const INTENT_STORE = "device";
const INTENT_KEY = "intent";
const OFF_COOKIE = "multica_push_off";

async function pushWanted() {
  try {
    if (self.cookieStore && (await self.cookieStore.get(OFF_COOKIE))) return false;
    const db = await new Promise((resolve, reject) => {
      const request = self.indexedDB.open(INTENT_DB, 1);
      request.onupgradeneeded = () => void request.result.createObjectStore(INTENT_STORE);
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error);
    });
    try {
      const value = await new Promise((resolve, reject) => {
        const read = db.transaction(INTENT_STORE, "readonly").objectStore(INTENT_STORE).get(INTENT_KEY);
        read.onsuccess = () => resolve(read.result);
        read.onerror = () => reject(read.error);
      });
      return value === "on";
    } finally {
      db.close();
    }
  } catch {
    return false;
  }
}

// The two CSRF cookies the API client echoes, session-bound first (client.ts).
const CSRF_COOKIES = ["multica_csrf_session", "multica_csrf"];

async function csrfValues() {
  const store = self.cookieStore;
  if (!store) return [];
  const values = [];
  for (const name of CSRF_COOKIES) {
    try {
      const cookie = await store.get(name);
      if (cookie && cookie.value) values.push(cookie.value);
    } catch {
      // Unreadable: try the next one.
    }
  }
  return values;
}

// True once the server holds this subscription.
async function registerWithServer(subscription) {
  const body = subscription && subscription.toJSON && subscription.toJSON();
  if (!body || !body.endpoint || !body.keys) return false;
  for (const token of await csrfValues()) {
    try {
      const response = await self.fetch("/api/push/subscriptions", {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json", "X-CSRF-Token": token },
        body: JSON.stringify({ endpoint: body.endpoint, keys: body.keys }),
      });
      if (response.ok) return true;
      // Only a CSRF rejection is worth retrying with the other cookie.
      if (response.status !== 403) return false;
    } catch {
      return false;
    }
  }
  return false;
}

// The page holds this lock for the whole opt-out and for its own registration
// (packages/core/push/push-intent.ts, same name), so a renewal never interleaves
// with either: it runs before the opt-out, which then removes the endpoint it
// registered, or after it, and sees the choice off. Without navigator.locks no
// ordering is possible, so the worker registers nothing and only tells open
// tabs; the app reconciles on its next load.
const DEVICE_LOCK = "multica-push-device";

async function tellTabs() {
  const tabs = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  tabs.forEach((tab) => tab.postMessage({ type: "push-subscription-changed" }));
}

self.addEventListener("pushsubscriptionchange", (event) => {
  const old = event.oldSubscription;
  const locks = self.navigator && self.navigator.locks;
  if (!locks) {
    event.waitUntil(tellTabs());
    return;
  }
  event.waitUntil(
    locks.request(DEVICE_LOCK, async () => {
      if (!(await pushWanted())) return;
      let renewed = event.newSubscription || null;
      try {
        const key = old && old.options && old.options.applicationServerKey;
        if (key) renewed = await self.registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
      } catch {
        // Keep what the browser handed over, if anything.
      }
      if (renewed && (await registerWithServer(renewed).catch(() => false))) return;
      await tellTabs();
    }),
  );
});
