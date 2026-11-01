"use client";

import { useQuery } from "@tanstack/react-query";
import { PauseCircle } from "@multica/ui/icons";
import { useCurrentWorkspace } from "@multica/core/paths";
import { memberListOptions } from "@multica/core/workspace/queries";
import { deriveWakeupsPause } from "@multica/core/workspace/wakeups-pause";
import { useViewingTimezone } from "../../common/use-viewing-timezone";
import { useLocale, useT } from "../../i18n";
import { formatWakeupTime } from "./wakeup-presentation";

/** Says who paused the workspace's wakeups and when; renders nothing while they run. */
export function WakeupsPausedBanner({ className = "" }: { className?: string }) {
  const { t } = useT("issues");
  const locale = useLocale();
  const viewTZ = useViewingTimezone();
  const workspace = useCurrentWorkspace();
  const { data: members = [] } = useQuery({ ...memberListOptions(workspace?.id ?? ""), enabled: !!workspace?.id });
  const pause = deriveWakeupsPause(workspace);
  if (!pause.paused) return null;
  const time = pause.at ? formatWakeupTime(pause.at, locale, viewTZ) : "";
  const name = members.find((m) => m.user_id === pause.by)?.name;
  return (
    <p
      role="status"
      data-testid="wakeups-paused-banner"
      className={`flex items-start gap-1.5 rounded-md border border-warning/30 bg-warning/10 px-3 py-2 text-caption leading-5 text-foreground ${className}`}
    >
      <PauseCircle className="mt-0.5 size-3.5 shrink-0 text-warning" aria-hidden="true" />
      <span className="min-w-0 break-words">
        {name
          ? t(($) => $.wakeups.paused_banner, { name, time })
          : t(($) => $.wakeups.paused_banner_anonymous, { time })}
      </span>
    </p>
  );
}
