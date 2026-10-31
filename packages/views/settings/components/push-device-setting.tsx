"use client";

import { Switch } from "@multica/ui/components/ui/switch";
import { isDesktopShell } from "../../platform";
import { useT } from "../../i18n";
import { SettingsCard, SettingsRow } from "./settings-layout";
import { usePushDevice, type PushDeviceState } from "./use-push-device";

const OFF_STATES: readonly PushDeviceState[] = ["off", "failed", "registering"];

/**
 * "Push on this device": requests permission from the tap, subscribes with the
 * deployment key and registers this browser. Web only — the desktop app has no
 * service worker and no Web Push.
 */
export function PushDeviceSetting() {
  const { t } = useT("settings");
  const { state, enable, disable } = usePushDevice();

  if (isDesktopShell() || state === "loading") return null;

  if (state === "needs-home-screen") {
    return (
      <SettingsCard>
        <SettingsRow
          anchor="push-device"
          label={t(($) => $.notifications.device.ios.title)}
          description={
            <ol className="mt-1 list-decimal space-y-0.5 pl-4">
              <li>{t(($) => $.notifications.device.ios.step_share)}</li>
              <li>{t(($) => $.notifications.device.ios.step_add)}</li>
              <li>{t(($) => $.notifications.device.ios.step_sign_in)}</li>
            </ol>
          }
        >
          <span />
        </SettingsRow>
      </SettingsCard>
    );
  }

  if (state === "unsupported") {
    return (
      <SettingsCard>
        <SettingsRow
          anchor="push-device"
          label={t(($) => $.notifications.device.push_label)}
          description={t(($) => $.notifications.device.unsupported)}
        >
          <span />
        </SettingsRow>
      </SettingsCard>
    );
  }

  const hint =
    state === "blocked"
      ? t(($) => $.notifications.device.blocked)
      : state === "not-configured"
        ? t(($) => $.notifications.device.not_configured)
        : state === "load-failed"
          ? t(($) => $.notifications.device.load_failed)
          : state === "failed"
            ? t(($) => $.notifications.device.failed)
            : undefined;
  const switchable = state === "on" || OFF_STATES.includes(state);

  return (
    <SettingsCard>
      <SettingsRow
        anchor="push-device"
        label={t(($) => $.notifications.device.push_label)}
        description={hint}
      >
        <Switch
          checked={state === "on"}
          disabled={!switchable || state === "registering"}
          aria-label={t(($) => $.notifications.device.push_label)}
          onCheckedChange={(checked) => void (checked ? enable() : disable())}
        />
      </SettingsRow>
    </SettingsCard>
  );
}
