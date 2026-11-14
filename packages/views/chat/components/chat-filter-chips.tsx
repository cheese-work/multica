"use client";

import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Pin } from "@multica/ui/icons";
import { cn } from "@multica/ui/lib/utils";
import type { ChatListView } from "@multica/core/chat/queries";
import { useT } from "../../i18n";

/** A count is `undefined` while unknown (loading); the chip then shows none, never 0. */
export type ChatFilterCounts = Partial<Record<ChatListView, number>>;

/** Chip slots in display order. `pin` stands for both Pinned and Not pinned. */
const SLOTS = ["all", "main", "decisions", "waiting", "unseen", "pin", "archived"] as const;
type Slot = (typeof SLOTS)[number];

const isPinView = (view: ChatListView) => view === "pinned" || view === "unpinned";

/**
 * Filter chips above the DM list. One tab stop (roving tabindex); arrows, Home
 * and End move focus; Enter or Space selects. When the chips overflow, the
 * wheel scrolls the row sideways until its end, then the list takes over.
 */
export function ChatFilterChips({
  value,
  counts = {},
  onChange,
}: {
  value: ChatListView;
  counts?: ChatFilterCounts;
  onChange: (view: ChatListView) => void;
}) {
  const { t } = useT("chat");
  const trackRef = useRef<HTMLDivElement>(null);
  const [overflowing, setOverflowing] = useState(false);
  const slotOf = (v: ChatListView): Slot => (isPinView(v) ? "pin" : (v as Slot));
  const [focused, setFocused] = useState<Slot>(slotOf(value));
  // Follow the selection unless focus is already inside the row.
  useEffect(() => {
    if (!trackRef.current?.contains(document.activeElement)) setFocused(slotOf(value));
  }, [value]);

  // Native listener: React's onWheel is passive and cannot cancel the page scroll.
  useEffect(() => {
    const el = trackRef.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      if (Math.abs(e.deltaX) >= Math.abs(e.deltaY)) return; // touchpad sideways: native
      const max = el.scrollWidth - el.clientWidth;
      if (max <= 0) return; // everything fits: the wheel belongs to the list
      const atEnd = e.deltaY > 0 ? el.scrollLeft >= max - 1 : el.scrollLeft <= 0;
      if (atEnd) return;
      el.scrollLeft = Math.min(max, Math.max(0, el.scrollLeft + e.deltaY));
      e.preventDefault();
    };
    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
  }, []);

  // Fade the edges only while something is hidden; start with "All" half hidden.
  useLayoutEffect(() => {
    const el = trackRef.current;
    if (!el) return;
    const measure = () => setOverflowing(el.scrollWidth > el.clientWidth);
    measure();
    if (el.scrollWidth > el.clientWidth && el.scrollLeft === 0) {
      const first = el.firstElementChild as HTMLElement | null;
      if (first) el.scrollLeft = first.offsetWidth / 2;
    }
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  const labelOf = (slot: Slot) =>
    slot === "pin"
      ? value === "unpinned"
        ? t(($) => $.list.filters.unpinned)
        : t(($) => $.list.filters.pinned)
      : t(($) => $.list.filters[slot]);

  const viewOf = (slot: Slot): ChatListView =>
    slot === "pin" ? (value === "pinned" ? "unpinned" : "pinned") : slot;

  const countOf = (slot: Slot) => (slot === "pin" ? undefined : counts[slot]);

  const buttons = () => Array.from(trackRef.current?.querySelectorAll<HTMLButtonElement>("button") ?? []);
  const moveFocus = (index: number) => {
    const list = buttons();
    const next = list[(index + list.length) % list.length];
    if (!next) return;
    setFocused(next.dataset.slot as Slot);
    next.focus();
    next.scrollIntoView?.({ inline: "nearest", block: "nearest" });
  };

  return (
    <div
      role="toolbar"
      aria-label={t(($) => $.list.filters.label)}
      data-overflowing={overflowing ? "true" : undefined}
      className={cn(
        "border-b border-surface-border pb-2.5",
        overflowing && "[mask-image:linear-gradient(90deg,transparent_0,#000_24px,#000_calc(100%-24px),transparent)]",
      )}
    >
      <div
        ref={trackRef}
        data-chip-track
        className="flex gap-1.5 overflow-x-auto px-2 py-0.5 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
        onKeyDown={(e) => {
          const list = buttons();
          const at = list.findIndex((b) => b === document.activeElement);
          if (at < 0) return;
          const key = e.key;
          if (key === "ArrowRight") moveFocus(at + 1);
          else if (key === "ArrowLeft") moveFocus(at - 1);
          else if (key === "Home") moveFocus(0);
          else if (key === "End") moveFocus(list.length - 1);
          else return;
          e.preventDefault();
        }}
      >
        {SLOTS.map((slot) => {
          const selected = slot === "pin" ? isPinView(value) : value === slot;
          const count = countOf(slot);
          const label = labelOf(slot);
          return (
            // ui-pattern-ref: allow toggle pill with roving tabindex and aria-pressed; Button has no pill-toggle variant
            <button
              key={slot}
              type="button"
              data-slot={slot}
              aria-pressed={selected}
              aria-label={count === undefined ? label : t(($) => $.list.filters.count_aria, { label, count })}
              tabIndex={focused === slot ? 0 : -1}
              onFocus={() => setFocused(slot)}
              onClick={() => onChange(viewOf(slot))}
              className={cn(
                "inline-flex h-9 shrink-0 items-center gap-1.5 whitespace-nowrap rounded-full border px-3 text-caption outline-none transition-colors motion-reduce:transition-none md:h-7 md:px-2.5",
                "focus-visible:ring-2 focus-visible:ring-ring",
                selected
                  ? "border-transparent bg-foreground font-semibold text-background"
                  : "border-border text-muted-foreground hover:bg-accent/50 hover:text-foreground",
              )}
            >
              {slot === "pin" && <Pin aria-hidden="true" className="size-3" />}
              {label}
              {count !== undefined && (
                <span
                  aria-hidden="true"
                  className={cn(
                    "tabular-nums",
                    selected ? "text-background" : slot === "waiting" && count > 0 ? "font-semibold text-destructive" : "text-faint-foreground",
                  )}
                >
                  {count}
                </span>
              )}
            </button>
          );
        })}
      </div>
    </div>
  );
}
