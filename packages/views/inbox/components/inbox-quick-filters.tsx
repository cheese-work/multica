"use client";

import { useInboxFilters, useInboxFilterStore } from "@multica/core/inbox/filter-store";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";

type QuickFilter = "all" | "unread" | "dm";

// Chips over the filter store's `unreadOnly` and `dmOnly`; exactly one shows
// as active. A menu-set combination of both reads as DMs, the narrower view.
export function InboxQuickFilters({ wsId }: { wsId: string }) {
  const { t } = useT("inbox");
  const filters = useInboxFilters(wsId);
  const setQuickFilter = useInboxFilterStore((state) => state.setQuickFilter);
  const active: QuickFilter = filters.dmOnly ? "dm" : filters.unreadOnly ? "unread" : "all";
  const chips: Array<[QuickFilter, string]> = [
    ["all", t(($) => $.filters.chip_all)],
    ["unread", t(($) => $.filters.chip_unread)],
    ["dm", t(($) => $.filters.chip_dm)],
  ];

  return (
    <div
      role="group"
      aria-label={t(($) => $.filters.quick_label)}
      className="flex shrink-0 flex-wrap items-center gap-1.5 px-3 py-2"
    >
      {chips.map(([mode, label]) => (
        <button
          key={mode}
          type="button"
          aria-pressed={active === mode}
          onClick={() => setQuickFilter(wsId, mode)}
          className={cn(
            "rounded-full border px-2.5 py-1 text-caption outline-none transition-colors focus-visible:ring-1 focus-visible:ring-ring",
            active === mode
              ? "border-transparent bg-accent font-medium text-foreground"
              : "text-muted-foreground hover:bg-accent/50 hover:text-foreground",
          )}
        >
          {label}
        </button>
      ))}
    </div>
  );
}
