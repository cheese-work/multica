import type { PushConfig, PushSubscriptionRequest } from "../types";
import {
  ensurePushRegistration,
  getPushSupport,
  getThisDeviceSubscription,
  registrationBody,
  subscribeThisDevice,
} from "./push-client";

export type PushSyncResult = "registered" | "skipped";

type Registrar = { registerPushSubscription: (body: PushSubscriptionRequest) => Promise<void> };

function isGranted(): boolean {
  return getPushSupport() === "ok" && window.Notification.permission === "granted";
}

/**
 * App load: hand the server the subscription this browser already holds. The
 * service worker cannot do it (cookie auth needs CSRF), and registering again
 * is what rebinds the subscription to the current login session. Never
 * prompts. Never throws, so a page load cannot break on it.
 */
export async function reRegisterThisDevice(api: Registrar): Promise<PushSyncResult> {
  if (!isGranted()) return "skipped";
  try {
    const subscription = await getThisDeviceSubscription();
    const body = subscription && registrationBody(subscription);
    if (!body) return "skipped";
    await api.registerPushSubscription(body);
    return "registered";
  } catch {
    return "skipped";
  }
}

/**
 * The browser replaced or dropped the subscription (`pushsubscriptionchange`):
 * subscribe again with the deployment key and register the new one.
 */
export async function resubscribeThisDevice(
  api: Registrar & { getPushConfig: () => Promise<PushConfig> },
): Promise<PushSyncResult> {
  if (!isGranted()) return "skipped";
  try {
    const config = await api.getPushConfig();
    if (!config.enabled || !config.public_key) return "skipped";
    const registration = await ensurePushRegistration();
    const body = await subscribeThisDevice(registration, config.public_key);
    if (!body) return "skipped";
    await api.registerPushSubscription(body);
    return "registered";
  } catch {
    return "skipped";
  }
}
