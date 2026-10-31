"use client";

import { useCallback, useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { getWebNotificationPermission, requestWebNotificationPermission } from "@multica/core/platform";
import {
  ensurePushRegistration,
  getPushSupport,
  getThisDeviceSubscription,
  setPushIntent,
  subscribeThisDevice,
  unsubscribeThisDevice,
  type PushSupport,
} from "@multica/core/push";
import { isDesktopShell } from "../../platform";

export type PushDeviceState =
  | "loading"
  | "unsupported"
  | "needs-home-screen"
  | "not-configured"
  | "blocked"
  | "load-failed"
  | "off"
  | "registering"
  | "on"
  | "failed"
  | "disable-failed";

/**
 * This browser's push registration. Browser support and permission come from
 * `window`, so the first paint stays "loading" until after mount (same
 * hydration rule as BrowserNotificationSetting).
 */
export function usePushDevice() {
  const [support, setSupport] = useState<PushSupport | null>(null);
  const [subscribed, setSubscribed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState<"enable" | "disable" | null>(null);
  // Bumped after a permission prompt so the blocked state is re-read.
  const [, setPermissionReads] = useState(0);

  useEffect(() => {
    if (isDesktopShell()) return;
    const current = getPushSupport();
    setSupport(current);
    if (current !== "ok" || getWebNotificationPermission() !== "granted") return;
    let live = true;
    getThisDeviceSubscription()
      .then((sub) => live && setSubscribed(sub !== null))
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);

  const config = useQuery({
    queryKey: ["push", "config"],
    queryFn: () => api.getPushConfig(),
    enabled: support === "ok",
    staleTime: 5 * 60_000,
  });

  const enable = useCallback(async () => {
    if (!config.data?.enabled) return;
    setBusy(true);
    setFailed(null);
    try {
      // First await in the tap handler, so the browser still sees a user gesture.
      const permission = await requestWebNotificationPermission();
      setPermissionReads((n) => n + 1);
      if (permission !== "granted") return;
      // The user's request, kept even if the steps below fail: the next app
      // load retries it (reRegisterThisDevice) instead of dropping it.
      setPushIntent("on");
      const registration = await ensurePushRegistration();
      const body = await subscribeThisDevice(registration, config.data.public_key);
      if (!body) throw new Error("subscription has no keys");
      await api.registerPushSubscription(body);
      setSubscribed(true);
    } catch {
      setFailed("enable");
    } finally {
      setBusy(false);
    }
  }, [config.data]);

  const disable = useCallback(async () => {
    setBusy(true);
    setFailed(null);
    try {
      const result = await unsubscribeThisDevice(api);
      // Read back what the browser still holds: a failed opt-out can leave it
      // subscribed, and the switch must not claim otherwise.
      const left = await getThisDeviceSubscription().catch(() => null);
      setSubscribed(left !== null);
      if (result === "failed") setFailed("disable");
    } finally {
      setBusy(false);
    }
  }, []);

  let state: PushDeviceState;
  if (support === null) state = "loading";
  else if (support !== "ok") state = support;
  else if (config.isError) state = "load-failed";
  else if (!config.data) state = "loading";
  else if (!config.data.enabled) state = "not-configured";
  else if (getWebNotificationPermission() === "denied") state = "blocked";
  else if (busy) state = "registering";
  else if (failed === "disable") state = "disable-failed";
  else if (failed === "enable") state = "failed";
  else state = subscribed ? "on" : "off";

  return { state, enable, disable };
}
