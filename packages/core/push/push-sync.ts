import type { PushConfig, PushSubscriptionRequest } from "../types";
import {
  ensurePushRegistration,
  getPushSupport,
  getThisDeviceSubscription,
  registrationBody,
  subscribeThisDevice,
} from "./push-client";
import { isPushWanted } from "./push-intent";

export type PushSyncResult = "registered" | "skipped";

type Registrar = { registerPushSubscription: (body: PushSubscriptionRequest) => Promise<void> };

function isGranted(): boolean {
  return getPushSupport() === "ok" && window.Notification.permission === "granted";
}

type Resubscriber = Registrar & { getPushConfig: () => Promise<PushConfig> };

/**
 * App load: hand the server the subscription this browser holds. The service
 * worker cannot do it (cookie auth needs CSRF), and registering again is what
 * rebinds the subscription to the current login session, and what teaches the
 * server an endpoint the worker renewed while no tab was open. Where the
 * browser dropped the subscription and the user still wants push, subscribe
 * again. Does nothing unless the stored choice is positively "on": after an
 * opt-out, or when the choice cannot be read, a retained subscription is not
 * registered. Never prompts. Never throws, so a page
 * load cannot break on it.
 */
export async function reRegisterThisDevice(api: Resubscriber): Promise<PushSyncResult> {
  if (!isGranted() || !(await isPushWanted())) return "skipped";
  try {
    const subscription = await getThisDeviceSubscription();
    if (!subscription) return await resubscribeThisDevice(api);
    const body = registrationBody(subscription);
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
export async function resubscribeThisDevice(api: Resubscriber): Promise<PushSyncResult> {
  if (!isGranted() || !(await isPushWanted())) return "skipped";
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
