import { useCallback } from "react";
import type { RuntimePauseReason } from "@multica/core/types";
import { useLocale, useT } from "../../i18n";
import { remainingParts } from "./machine-pause-state";

/** Reason, time-left and clock text shared by the machine card and the owner banner. */
export function useMachinePauseFormat() {
  const { t } = useT("runtimes");
  const locale = useLocale();

  const reasonName = useCallback(
    (reason: RuntimePauseReason) => t(($) => $.machine_pause.reasons[reason].name),
    [t],
  );

  const duration = useCallback(
    (ms: number) => {
      const { hours, minutes } = remainingParts(ms);
      if (hours > 0 && minutes > 0) {
        return t(($) => $.machine_pause.duration.hours_minutes, { hours, minutes });
      }
      return hours > 0
        ? t(($) => $.machine_pause.duration.hours, { hours })
        : t(($) => $.machine_pause.duration.minutes, { minutes });
    },
    [t],
  );

  // The viewer's own zone, named, so "16:20 GMT+7" cannot be read as another zone.
  const clock = useCallback(
    (iso: string) => {
      const at = new Date(iso);
      if (Number.isNaN(at.getTime())) return iso;
      return new Intl.DateTimeFormat(locale, {
        hour: "numeric",
        minute: "2-digit",
        timeZoneName: "short",
      }).format(at);
    },
    [locale],
  );

  return { reasonName, duration, clock };
}
