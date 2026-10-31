/* global self, URL */
// Web Push worker (CHE-1798). Payloads are content-private: sender, a fixed
// label and a url, never message text. Plain JS because Next serves public/
// as is. Registration needs the CSRF-protected API, which the worker cannot
// call, so a renewal here only tells open tabs. With none open (or a renewal
// that failed) the app reconciles on its next load: it registers the
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

self.addEventListener("pushsubscriptionchange", (event) => {
  const old = event.oldSubscription;
  event.waitUntil(
    (async () => {
      try {
        const key = old && old.options && old.options.applicationServerKey;
        if (key) await self.registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
      } catch {
        // The app subscribes again with the deployment key on its next load.
      }
      const tabs = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
      tabs.forEach((tab) => tab.postMessage({ type: "push-subscription-changed" }));
    })(),
  );
});
