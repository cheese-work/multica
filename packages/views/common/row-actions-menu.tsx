"use client";

import { Fragment, type KeyboardEvent, type ReactNode } from "react";
import { MoreHorizontal } from "@multica/ui/icons";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuShortcut,
  DropdownMenuTrigger,
} from "@multica/ui/components/ui/dropdown-menu";
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuSeparator,
  ContextMenuShortcut,
  ContextMenuTrigger,
} from "@multica/ui/components/ui/context-menu";
import { cn } from "@multica/ui/lib/utils";

export interface RowActionItem {
  key: string;
  icon: ReactNode;
  label: string;
  onSelect: () => void;
  danger?: boolean;
  /** Single-key hint shown in the menu; the row binds the key (see `handleRowShortcut`). */
  shortcut?: string;
}

/**
 * Enter/Space activation for a `role="button"` row that hosts its own
 * controls (this menu, inline buttons): a key pressed inside one of those
 * controls belongs to that control, not to the row.
 */
export function handleRowActivationKey(
  e: KeyboardEvent<HTMLElement>,
  activate: () => void,
) {
  if (e.target !== e.currentTarget) return;
  if (e.key !== "Enter" && e.key !== " ") return;
  e.preventDefault();
  activate();
}

/**
 * A list row's actions, collected behind one always-visible trigger.
 *
 * This is the touch counterpart of the hover controls list rows reveal on a
 * pointer surface: a coarse pointer has neither hover nor right-click, so it
 * would otherwise never reach them. It renders where the primary pointer
 * cannot hover and hides where it can, letting the hover controls take over —
 * the two must therefore offer the same actions.
 *
 * The switch is `hover`, not a width breakpoint: a phone in landscape clears
 * `md` while still having no hover, which would otherwise leave it with the
 * hover controls it can never trigger.
 *
 * `groups` are rendered in order with a separator between them.
 */
export function RowActionsMenu({
  label,
  groups,
  revealOnHover = false,
}: {
  /** Accessible name of the trigger, e.g. "Chat actions". */
  label: string;
  groups: RowActionItem[][];
  /**
   * Where the pointer can hover, show the trigger only while the enclosing
   * `group/row` is hovered or has focus, instead of replacing it with the
   * caller's own hover controls. Without hover it stays always visible.
   */
  revealOnHover?: boolean;
}) {
  const filled = groups.filter((group) => group.length > 0);
  if (filled.length === 0) return null;

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <button
            type="button"
            aria-label={label}
            // The row owns click-to-select; opening the menu must not select it.
            onPointerDown={(e) => e.stopPropagation()}
            onClick={(e) => e.stopPropagation()}
            className={cn(
              "inline-flex size-8 shrink-0 items-center justify-center rounded-xs text-muted-foreground outline-none transition-colors hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring data-popup-open:bg-accent data-popup-open:text-foreground",
              revealOnHover
                ? "[@media(hover:hover)]:invisible [@media(hover:hover)]:group-hover/row:visible [@media(hover:hover)]:group-focus-within/row:visible data-popup-open:visible"
                : "[@media(hover:hover)]:hidden",
            )}
          >
            <MoreHorizontal className="size-4" />
          </button>
        }
      />
      <DropdownMenuContent align="end" className="w-auto">
        {filled.map((group, index) => (
          <Fragment key={group[0]!.key}>
            {index > 0 && <DropdownMenuSeparator />}
            {group.map((item) => (
              <DropdownMenuItem
                key={item.key}
                variant={item.danger ? "destructive" : "default"}
                onClick={item.onSelect}
              >
                {item.icon}
                {item.label}
                {item.shortcut && <DropdownMenuShortcut>{item.shortcut}</DropdownMenuShortcut>}
              </DropdownMenuItem>
            ))}
          </Fragment>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/**
 * Right-click opens the same groups `RowActionsMenu` offers. Wraps the row, so
 * the row's own handlers stay on `children`.
 */
export function RowContextMenu({
  groups,
  children,
  ...rowProps
}: {
  groups: RowActionItem[][];
  children: ReactNode;
} & Omit<React.ComponentProps<"div">, "children">) {
  const filled = groups.filter((group) => group.length > 0);
  if (filled.length === 0) return <div {...rowProps}>{children}</div>;

  return (
    <ContextMenu>
      <ContextMenuTrigger render={<div {...rowProps} />}>{children}</ContextMenuTrigger>
      <ContextMenuContent className="w-auto" side="bottom" align="start">
        {filled.map((group, index) => (
          <Fragment key={group[0]!.key}>
            {index > 0 && <ContextMenuSeparator />}
            {group.map((item) => (
              <ContextMenuItem
                key={item.key}
                variant={item.danger ? "destructive" : "default"}
                onClick={item.onSelect}
              >
                {item.icon}
                {item.label}
                {item.shortcut && <ContextMenuShortcut>{item.shortcut}</ContextMenuShortcut>}
              </ContextMenuItem>
            ))}
          </Fragment>
        ))}
      </ContextMenuContent>
    </ContextMenu>
  );
}
