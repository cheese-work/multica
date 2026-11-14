"use client";

import { useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { Archive, ArchiveRestore, ChevronDown, Eye, EyeOff, Pin, PinOff } from "@multica/ui/icons";
import { Button } from "@multica/ui/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@multica/ui/components/ui/dropdown-menu";
import { TooltipHint } from "@multica/ui/components/common/tooltip-hint";
import { cn } from "@multica/ui/lib/utils";
import type { ChatSession } from "@multica/core/types";
import { useT } from "../../i18n";
import { planBulk, type BulkAction } from "./use-chat-selection";

const ICON: Record<BulkAction, ReactNode> = {
  seen: <Eye />,
  unseen: <EyeOff />,
  pin: <Pin className="-rotate-45" />,
  unpin: <PinOff />,
  archive: <Archive />,
  unarchive: <ArchiveRestore />,
};

/**
 * True while the labelled bar overflows its column (CHE-5883): the bar then
 * draws icons only, so no button is ever clipped. It remembers the width the
 * labelled bar needed, and goes back to labels once the column is that wide.
 */
function useOverflowFallback(ref: React.RefObject<HTMLElement | null>) {
  const [narrow, setNarrow] = useState(false);
  const needed = useRef(0);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const check = () => {
      if (narrow) {
        if (el.clientWidth >= needed.current) setNarrow(false);
      } else if (el.scrollWidth > el.clientWidth) {
        needed.current = el.scrollWidth;
        setNarrow(true);
      }
    };
    check();
    const ro = new ResizeObserver(check);
    ro.observe(el);
    return () => ro.disconnect();
  }, [ref, narrow]);
  return narrow;
}

/**
 * Bulk actions for the selected DMs: three split buttons `[action | ▾]`, 28 px
 * high with short labels as in the CHE-5511 lock. The arrow offers the
 * opposite action with its count. There is no bulk delete. Icons only (action
 * name in `aria-label` and a tooltip) on the phone (`compact`, 44×44) and
 * whenever the labels do not fit the column.
 */
export function ChatBulkBar({
  sessions,
  compact = false,
  onAction,
}: {
  sessions: ChatSession[];
  compact?: boolean;
  onAction: (action: BulkAction, ids: string[]) => void;
}) {
  const { t } = useT("chat");
  const plan = planBulk(sessions);
  const ref = useRef<HTMLDivElement>(null);
  const narrow = useOverflowFallback(ref);
  const iconOnly = compact || narrow;
  const touch = compact ? "size-11" : "";

  return (
    <div
      ref={ref}
      role="toolbar"
      aria-label={t(($) => $.list.bulk.toolbar_aria, { count: sessions.length })}
      className={cn(
        "mx-2 mb-2 flex items-center gap-[5px] rounded-xl border bg-popover p-[7px] shadow-lg [&>:first-child]:ml-auto",
        compact && "justify-around gap-2 [&>:first-child]:ml-0",
      )}
    >
      {(["seen", "pin", "archive"] as const).map((kind) => {
        const { primary, opposite } = plan[kind];
        const label = t(($) => $.list.bulk.action[primary.action]);
        const visible =
          primary.action === "seen"
            ? t(($) => $.list.bulk.short.seen)
            : primary.action === "unseen"
              ? t(($) => $.list.bulk.short.unseen)
              : label;
        const oppositeLabel = `${t(($) => $.list.bulk.action[opposite.action])} (${opposite.ids.length})`;
        return (
          <div key={kind} className="inline-flex shrink-0">
            <TooltipHint label={iconOnly ? label : null}>
              <Button
                type="button"
                size="sm"
                variant={kind === "archive" ? "default" : "outline"}
                disabled={primary.ids.length === 0}
                aria-label={label}
                onClick={() => onAction(primary.action, primary.ids)}
                className={cn(
                  "gap-[5px] rounded-r-none px-1.5 text-caption! [&_svg:not([class*='size-'])]:size-3.5",
                  touch,
                  iconOnly && "px-0",
                  iconOnly && !compact && "w-7",
                )}
              >
                {ICON[primary.action]}
                {!iconOnly && visible}
              </Button>
            </TooltipHint>
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    disabled={opposite.ids.length === 0}
                    aria-label={t(($) => $.list.bulk.more_aria, { action: oppositeLabel })}
                    className={cn("rounded-l-none border-l-0 px-[5px] text-caption!", touch)}
                  />
                }
              >
                <ChevronDown className="size-3" />
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" side="top">
                <DropdownMenuItem onClick={() => onAction(opposite.action, opposite.ids)}>
                  {ICON[opposite.action]}
                  {oppositeLabel}
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        );
      })}
    </div>
  );
}
