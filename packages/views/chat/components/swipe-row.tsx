"use client";

import {
  useEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type PointerEvent as ReactPointerEvent,
  type ReactNode,
} from "react";
import { cn } from "@multica/ui/lib/utils";

export interface SwipeAction {
  key: string;
  /** Accessible name; also the visible caption under the icon. */
  label: string;
  icon: ReactNode;
  onSelect: () => void;
  tone?: "neutral" | "info" | "warning" | "success";
}

const ACTION_WIDTH = 68;
const RIGHT_WIDTH = 96;
/** Drag distance that opens a tray instead of snapping back. */
const REVEAL_THRESHOLD = 48;
/** Movement that turns a press into a drag (and cancels a long press). */
const SLOP = 8;
const LONG_PRESS_MS = 500;
/** A click this soon after a drag or long press is its tail, not a tap. */
const CLICK_TAIL_MS = 100;
const FALLBACK_WIDTH = 360;

const TONE: Record<NonNullable<SwipeAction["tone"]>, string> = {
  neutral: "bg-muted text-foreground",
  info: "bg-info/15 text-info",
  warning: "bg-warning/15 text-warning",
  success: "bg-success/15 text-success",
};

type Side = "left" | "right" | null;

/**
 * Touch-only swipe shell around a list row: swipe left reveals `leftActions`
 * (a full swipe runs `onFullSwipeLeft`), swipe right reveals `rightAction`, and
 * a still press of 500 ms runs `onLongPress`. Mouse pointers pass straight
 * through, so desktop rows behave as before. Vertical movement is left to the
 * browser (`touch-pan-y`) and cancels the gesture, so the list still scrolls.
 *
 * Revealed actions are real buttons with accessible names and 44×44 hit
 * areas. A tap on the row while a tray is open only closes it.
 */
export function SwipeRow({
  children,
  leftActions,
  rightAction,
  onFullSwipeLeft,
  onLongPress,
}: {
  children: ReactNode;
  leftActions: SwipeAction[];
  rightAction?: SwipeAction;
  onFullSwipeLeft?: () => void;
  onLongPress?: () => void;
}) {
  const rootRef = useRef<HTMLDivElement>(null);
  const [offset, setOffset] = useState(0);
  const [dragging, setDragging] = useState(false);
  const [open, setOpen] = useState<Side>(null);

  const gesture = useRef<{
    id: number;
    x: number;
    y: number;
    mode: "pending" | "swiping" | "scroll" | "pressed";
    base: number;
  } | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const suppressUntil = useRef(0);

  const leftWidth = leftActions.length * ACTION_WIDTH;
  const rightWidth = rightAction ? RIGHT_WIDTH : 0;
  const restOffset = (side: Side) => (side === "left" ? -leftWidth : side === "right" ? rightWidth : 0);

  const clearTimer = () => {
    if (timer.current) clearTimeout(timer.current);
    timer.current = null;
  };
  useEffect(() => clearTimer, []);

  const close = () => {
    setOpen(null);
    setOffset(0);
  };

  const onPointerDown = (e: ReactPointerEvent<HTMLDivElement>) => {
    if (e.pointerType === "mouse" || e.isPrimary === false) return;
    gesture.current = {
      id: e.pointerId,
      x: e.clientX,
      y: e.clientY,
      mode: "pending",
      base: restOffset(open),
    };
    clearTimer();
    if (onLongPress && open === null) {
      timer.current = setTimeout(() => {
        const g = gesture.current;
        if (!g || g.mode !== "pending") return;
        g.mode = "pressed";
        onLongPress();
      }, LONG_PRESS_MS);
    }
  };

  const onPointerMove = (e: ReactPointerEvent<HTMLDivElement>) => {
    const g = gesture.current;
    if (!g || g.id !== e.pointerId) return;
    const dx = e.clientX - g.x;
    const dy = e.clientY - g.y;
    if (g.mode === "pending") {
      if (Math.abs(dx) <= SLOP && Math.abs(dy) <= SLOP) return;
      clearTimer();
      if (Math.abs(dy) > Math.abs(dx)) {
        g.mode = "scroll";
        return;
      }
      g.mode = "swiping";
      setDragging(true);
      try {
        e.currentTarget.setPointerCapture?.(e.pointerId);
      } catch {
        // Capture is a nicety; the gesture still works through bubbling.
      }
    }
    if (g.mode !== "swiping") return;
    const width = rootRef.current?.offsetWidth || FALLBACK_WIDTH;
    const min = leftWidth > 0 ? -width : 0;
    const max = rightWidth;
    setOffset(Math.min(max, Math.max(min, g.base + dx)));
  };

  const finish = (e: ReactPointerEvent<HTMLDivElement>, cancelled: boolean) => {
    const g = gesture.current;
    if (!g || g.id !== e.pointerId) return;
    gesture.current = null;
    clearTimer();
    setDragging(false);
    if (g.mode === "pressed") {
      suppressUntil.current = Date.now() + CLICK_TAIL_MS;
      return;
    }
    if (g.mode !== "swiping") return;
    suppressUntil.current = Date.now() + CLICK_TAIL_MS;
    if (cancelled) {
      setOffset(restOffset(open));
      return;
    }
    const width = rootRef.current?.offsetWidth || FALLBACK_WIDTH;
    const full = Math.max(width * 0.6, leftWidth + 40);
    if (onFullSwipeLeft && offset <= -full) {
      close();
      onFullSwipeLeft();
    } else if (leftWidth > 0 && offset <= -REVEAL_THRESHOLD) {
      setOpen("left");
      setOffset(-leftWidth);
    } else if (rightWidth > 0 && offset >= REVEAL_THRESHOLD) {
      setOpen("right");
      setOffset(rightWidth);
    } else {
      close();
    }
  };

  const onClickCapture = (e: React.MouseEvent) => {
    if (Date.now() < suppressUntil.current || open !== null) {
      e.stopPropagation();
      e.preventDefault();
      if (Date.now() >= suppressUntil.current) close();
    }
  };

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === "Escape" && open !== null) {
      e.stopPropagation();
      close();
    }
  };

  const run = (action: SwipeAction) => {
    close();
    action.onSelect();
  };

  const showLeft = offset < 0;
  const showRight = offset > 0;

  return (
    <div ref={rootRef} className="relative overflow-hidden" onKeyDown={onKeyDown}>
      {showLeft && (
        <div className="absolute inset-y-0 right-0 flex">
          {leftActions.map((action) => (
            <ActionButton key={action.key} action={action} width={ACTION_WIDTH} onRun={run} />
          ))}
        </div>
      )}
      {showRight && rightAction && (
        <div className="absolute inset-y-0 left-0 flex">
          <ActionButton action={rightAction} width={RIGHT_WIDTH} onRun={run} />
        </div>
      )}
      <div
        style={{ transform: offset === 0 ? undefined : `translateX(${offset}px)` }}
        className={cn(
          "relative touch-pan-y [@media(hover:none)]:select-none [@media(hover:none)]:[-webkit-touch-callout:none]",
          offset !== 0 && "bg-background",
          !dragging && "transition-transform duration-150 motion-reduce:transition-none",
        )}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={(e) => finish(e, false)}
        onPointerCancel={(e) => finish(e, true)}
        onClickCapture={onClickCapture}
      >
        {children}
      </div>
    </div>
  );
}

function ActionButton({
  action,
  width,
  onRun,
}: {
  action: SwipeAction;
  width: number;
  onRun: (action: SwipeAction) => void;
}) {
  return (
    <button
      type="button"
      aria-label={action.label}
      style={{ width }}
      onClick={() => onRun(action)}
      className={cn(
        "flex min-h-11 min-w-11 flex-col items-center justify-center gap-0.5 text-xs font-semibold outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring",
        TONE[action.tone ?? "neutral"],
      )}
    >
      {action.icon}
      <span aria-hidden="true">{action.label}</span>
    </button>
  );
}
