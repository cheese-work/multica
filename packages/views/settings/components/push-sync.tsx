"use client";

import { useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import { useWorkspaceId } from "@multica/core/hooks";
import { notificationChannelOptions } from "@multica/core/notification-preferences/channels";
import { getWebNotificationPermission } from "@multica/core/platform";
import { reRegisterThisDevice, resubscribeThisDevice } from "@multica/core/push";
import { useT } from "../../i18n";
import { isDesktopShell } from "../../platform";

/**
 * Web-only upkeep for Web Push, mounted once in the dashboard. It renders
 * nothing and never runs in Electron.
 *
 * - On load, re-registers the subscription this browser already holds, which
 *   rebinds it to the current login session (the worker cannot: cookie auth
 *   needs CSRF).
 * - Re-subscribes when the worker reports `pushsubscriptionchange`.
 * - Tells the user when a Push switch is on but the browser blocks
 *   notifications, because the switch then delivers nothing.
 */
export function PushSync() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const desktop = isDesktopShell();
  const { data } = useQuery({ ...notificationChannelOptions(wsId), enabled: !desktop });
  const pushWanted = data?.channels.inbox.push === true || data?.channels.dm.push === true;

  useEffect(() => {
    if (desktop) return;
    void reRegisterThisDevice(api);
    const serviceWorker = typeof navigator === "undefined" ? undefined : navigator.serviceWorker;
    if (!serviceWorker) return;
    const onMessage = (event: { data?: { type?: string } }) => {
      if (event.data?.type === "push-subscription-changed") void resubscribeThisDevice(api);
    };
    serviceWorker.addEventListener("message", onMessage);
    return () => serviceWorker.removeEventListener("message", onMessage);
  }, [desktop]);

  const blocked = pushWanted && getWebNotificationPermission() === "denied";
  useEffect(() => {
    if (desktop || !blocked) return;
    toast.warning(t(($) => $.notifications.device.blocked_banner), {
      id: "push-permission-blocked",
      duration: Infinity,
    });
  }, [desktop, blocked, t]);

  return null;
}
