"use client";

import { Server } from "lucide-react";
import { TooltipHint } from "@multica/ui/components/common/tooltip-hint";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../i18n";
import { PageHeaderTrailingContext } from "./page-header";
import { MACHINE_BANNER_ID, useMachineBanner } from "./use-machine-banner";

/**
 * Header icon for the machine banner: shown while a machine affects the viewer,
 * also after the banner was dismissed. The number is how many machines; the
 * filled square and `aria-pressed` tell an open banner from a dismissed one.
 */
export function MachineBannerToggle() {
  const { t } = useT("runtimes");
  const { machines, open, closeBanner, reopenBanner } = useMachineBanner();
  const first = machines[0];
  if (!first) return null;

  const summary =
    machines.length > 1
      ? t(($) => $.machine_pause.banner.summary, { count: machines.length })
      : first.state === "offline"
        ? t(($) => $.machine_pause.banner.icon_offline, { machine: first.title })
        : t(($) => $.machine_pause.banner.icon_paused, { machine: first.title });
  const label = open
    ? t(($) => $.machine_pause.banner.hide, { summary })
    : t(($) => $.machine_pause.banner.show, { summary });

  return (
    <TooltipHint label={label}>
      <button
        type="button"
        aria-label={label}
        aria-pressed={open}
        data-testid="machine-banner-toggle"
        onClick={() => {
          if (open) {
            closeBanner();
            return;
          }
          reopenBanner();
          // The banner mounts on the next render; move focus once it exists.
          requestAnimationFrame(() => document.getElementById(MACHINE_BANNER_ID)?.focus());
        }}
        className={cn(
          "inline-flex h-7 shrink-0 cursor-pointer items-center gap-1 rounded-md px-1.5 text-caption font-medium text-warning transition-colors hover:bg-accent sm:h-7 max-sm:h-10 max-sm:px-2.5",
          open && "bg-warning/15",
        )}
      >
        <Server aria-hidden="true" className="size-4" />
        <span>{machines.length > 9 ? "9+" : machines.length}</span>
      </button>
    </TooltipHint>
  );
}

/** Puts the machine banner icon at the right end of every `PageHeader` below it. */
export function MachineBannerHeaderSlot({ children }: { children: React.ReactNode }) {
  return (
    <PageHeaderTrailingContext.Provider value={<MachineBannerToggle />}>
      {children}
    </PageHeaderTrailingContext.Provider>
  );
}
