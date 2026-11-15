"use client";

import type { ReactNode } from "react";
import { Archive, ArchiveRestore, ChevronDown, Eye, EyeOff, Pin, PinOff } from "@multica/ui/icons";
import { Button } from "@multica/ui/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@multica/ui/components/ui/dropdown-menu";
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
 * Bulk actions for the selected DMs: three split buttons `[action | ▾]`. The
 * label follows the selection; the arrow offers the opposite action with its
 * count. There is no bulk delete. `compact` (phone) draws icons only, 44×44.
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
  const touch = compact ? "size-11" : "";

  return (
    <div
      role="toolbar"
      aria-label={t(($) => $.list.bulk.toolbar_aria, { count: sessions.length })}
      className={cn("flex items-center justify-end gap-2 border-t bg-background p-2", compact && "justify-around")}
    >
      {(["seen", "pin", "archive"] as const).map((kind) => {
        const { primary, opposite } = plan[kind];
        const label = t(($) => $.list.bulk.action[primary.action]);
        const oppositeLabel = `${t(($) => $.list.bulk.action[opposite.action])} (${opposite.ids.length})`;
        return (
          <div key={kind} className="inline-flex">
            <Button
              type="button"
              variant={kind === "archive" ? "default" : "outline"}
              disabled={primary.ids.length === 0}
              aria-label={compact ? label : undefined}
              onClick={() => onAction(primary.action, primary.ids)}
              className={cn("rounded-r-none", touch, compact && "px-0")}
            >
              {ICON[primary.action]}
              {!compact && label}
            </Button>
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <Button
                    type="button"
                    variant="outline"
                    disabled={opposite.ids.length === 0}
                    aria-label={t(($) => $.list.bulk.more_aria, { action: oppositeLabel })}
                    className={cn("rounded-l-none border-l-0 px-1.5", touch)}
                  />
                }
              >
                <ChevronDown />
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
