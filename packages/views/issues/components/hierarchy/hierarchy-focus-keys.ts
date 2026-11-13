import type { FocusItem } from "./hierarchy-focus-layout";

export type Direction = "left" | "right" | "up" | "down";
export type FocusAction =
  | { type: "step-in" }
  | { type: "step-out" }
  | { type: "toggle" }
  | { type: "open" }
  | { type: "move"; direction: Direction };

const ARROWS: Record<string, Direction> = { ArrowLeft: "left", ArrowRight: "right", ArrowUp: "up", ArrowDown: "down" };

/**
 * Key bindings of the focus-depth canvas. `null` means "not handled": the
 * caller leaves the event alone, so Escape at the root still closes the dialog.
 */
export function focusKeyAction(event: { key: string; ctrlKey?: boolean; metaKey?: boolean; altKey?: boolean }, depth: number): FocusAction | null {
  if (event.ctrlKey || event.metaKey || event.altKey) return null;
  const { key } = event;
  if (key === "+" || key === "=") return { type: "step-in" };
  if (key === "-" || key === "_" || key === "Escape") return depth > 0 ? { type: "step-out" } : null;
  if (key === "Enter") return { type: "open" };
  if (key === " ") return { type: "toggle" };
  const direction = ARROWS[key];
  return direction ? { type: "move", direction } : null;
}

/** Index of the nearest card from `from` in `direction`, or `from` when there is none. Only cards are stops. */
export function nextCard(items: FocusItem[], from: number, direction: Direction): number {
  const origin = items[from]?.box;
  if (!origin) return from;
  const centre = (b: typeof origin) => ({ x: b.x + b.w / 2, y: b.y + b.h / 2 });
  const o = centre(origin);
  const horizontal = direction === "left" || direction === "right";
  const sign = direction === "right" || direction === "down" ? 1 : -1;
  let best = from;
  let bestScore = Infinity;
  items.forEach((item, index) => {
    if (item.kind !== "card" || index === from) return;
    const c = centre(item.box);
    const [along, across] = horizontal ? [(c.x - o.x) * sign, c.y - o.y] : [(c.y - o.y) * sign, c.x - o.x];
    if (along <= 0) return;
    const score = along + Math.abs(across) * 2;
    if (score < bestScore) { best = index; bestScore = score; }
  });
  return best;
}
