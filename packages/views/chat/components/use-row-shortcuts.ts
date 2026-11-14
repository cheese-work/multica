import type { KeyboardEvent } from "react";
import type { RowActionItem } from "../../common/row-actions-menu";

function isEditable(target: EventTarget | null): boolean {
  const el = target as { tagName?: string; isContentEditable?: boolean } | null;
  if (!el) return false;
  return (
    el.isContentEditable === true ||
    el.tagName === "INPUT" ||
    el.tagName === "TEXTAREA" ||
    el.tagName === "SELECT"
  );
}

/**
 * Single-key row shortcuts: runs the item whose `shortcut` matches the key.
 * Typing in a field and any held modifier leave the key alone.
 */
export function handleRowShortcut(
  e: KeyboardEvent<HTMLElement>,
  items: Pick<RowActionItem, "shortcut" | "onSelect">[],
): void {
  if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey) return;
  if (isEditable(e.target)) return;
  const key = e.key.toUpperCase();
  const item = items.find((i) => i.shortcut === key);
  if (!item) return;
  e.preventDefault();
  item.onSelect();
}
