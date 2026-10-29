/**
 * Pure gesture logic for the zoom canvas: pointer sessions (pan + pinch),
 * wheel classification and the Safari `gesture*` adapter. DOM-free so every
 * regression is pinned by a unit test; `use-zoom-canvas` only forwards events.
 */

import {
  clampScale,
  clampTransform,
  computeMinScale,
  distanceBetween,
  midpointOf,
  zoomToAt,
  type Point,
  type Size,
  type ZoomTransform,
} from "./zoom-transform";

export interface GestureEnv {
  content: Size;
  viewport: Size;
}

export interface PointerSession {
  /** Active pointers in the order they went down (pointerId order is not stable). */
  pointers: ReadonlyArray<{ id: number; point: Point }>;
  /** Re-taken on every pointer-set change, from the then-current transform. */
  baseline: { transform: ZoomTransform; midpoint: Point; distance: number } | null;
}

export const EMPTY_POINTER_SESSION: PointerSession = { pointers: [], baseline: null };

function takeBaseline(
  pointers: PointerSession["pointers"],
  transform: ZoomTransform,
): PointerSession["baseline"] {
  const [first, second] = pointers;
  if (!first) return null;
  if (!second) return { transform, midpoint: first.point, distance: 0 };
  return {
    transform,
    midpoint: midpointOf(first.point, second.point),
    distance: distanceBetween(first.point, second.point),
  };
}

export function pointerDown(
  session: PointerSession,
  id: number,
  point: Point,
  transform: ZoomTransform,
): PointerSession {
  const pointers = session.pointers.some((p) => p.id === id)
    ? session.pointers.map((p) => (p.id === id ? { id, point } : p))
    : [...session.pointers, { id, point }];
  return { pointers, baseline: takeBaseline(pointers, transform) };
}

export function pointerMove(
  session: PointerSession,
  id: number,
  point: Point,
  env: GestureEnv,
): { session: PointerSession; transform: ZoomTransform | null } {
  const { baseline } = session;
  if (!baseline || !session.pointers.some((p) => p.id === id)) {
    return { session, transform: null };
  }
  const pointers = session.pointers.map((p) => (p.id === id ? { id, point } : p));
  const next: PointerSession = { pointers, baseline };
  const { transform: t0, midpoint: mid0, distance: d0 } = baseline;
  const [first, second] = pointers;

  if (!second || d0 <= 0) {
    return {
      session: next,
      transform: clampTransform(
        { scale: t0.scale, x: t0.x + first!.point.x - mid0.x, y: t0.y + first!.point.y - mid0.y },
        env.content,
        env.viewport,
      ),
    };
  }

  // Scale is clamped first and the anchor is solved from the clamped value, so
  // at the scale limits the midpoint still pans instead of the move being lost.
  const mid = midpointOf(first!.point, second.point);
  const scale = clampScale(
    (t0.scale * distanceBetween(first!.point, second.point)) / d0,
    computeMinScale(env.content, env.viewport),
  );
  const ratio = scale / t0.scale;
  return {
    session: next,
    transform: clampTransform(
      { scale, x: mid.x - (mid0.x - t0.x) * ratio, y: mid.y - (mid0.y - t0.y) * ratio },
      env.content,
      env.viewport,
    ),
  };
}

/** Covers both `pointerup` and `pointercancel`: only that pointer leaves. */
export function pointerUp(
  session: PointerSession,
  id: number,
  transform: ZoomTransform,
): PointerSession {
  if (!session.pointers.some((p) => p.id === id)) return session;
  const pointers = session.pointers.filter((p) => p.id !== id);
  return { pointers, baseline: takeBaseline(pointers, transform) };
}

export type WheelAction =
  | { type: "zoom"; factor: number }
  | { type: "pan"; dx: number; dy: number };

export interface WheelLike {
  deltaX: number;
  deltaY: number;
  deltaMode: number;
  ctrlKey: boolean;
  metaKey: boolean;
  shiftKey: boolean;
}

// Per-event clamp and divisor for ctrl/meta wheel zoom: one mouse notch
// (deltaY 100) clamps to 18px, exp(0.18) ~= ZOOM_STEP, while a trackpad pinch
// (small deltas) stays unclamped. Tune on device.
const WHEEL_ZOOM_CLAMP_PX = 18;
const WHEEL_ZOOM_DIVISOR = 100;

// Mice report lines (1) or pages (2); trackpads report pixels (0).
function toPixels(delta: number, deltaMode: number): number {
  return deltaMode === 1 ? delta * 16 : deltaMode === 2 ? delta * 100 : delta;
}

export function classifyWheel(event: WheelLike): WheelAction {
  const dy = toPixels(event.deltaY, event.deltaMode);
  if (event.ctrlKey || event.metaKey) {
    const pixels = Math.max(-WHEEL_ZOOM_CLAMP_PX, Math.min(WHEEL_ZOOM_CLAMP_PX, dy));
    return { type: "zoom", factor: Math.exp(-pixels / WHEEL_ZOOM_DIVISOR) };
  }
  const dx = toPixels(event.deltaX, event.deltaMode);
  if (event.shiftKey && dx === 0) return { type: "pan", dx: -dy, dy: 0 };
  return { type: "pan", dx: -dx, dy: -dy };
}

export interface SafariGestureState {
  active: boolean;
  /** `performance.now()`-style ms of the last `gestureend`, or null. */
  endedAt: number | null;
  baseline: { transform: ZoomTransform; anchor: Point } | null;
}

export const IDLE_SAFARI_GESTURE: SafariGestureState = {
  active: false,
  endedAt: null,
  baseline: null,
};

export function gestureStart(
  state: SafariGestureState,
  transform: ZoomTransform,
  anchor: Point,
): SafariGestureState {
  return { active: true, endedAt: state.endedAt, baseline: { transform, anchor } };
}

/** Null when the event must not move the canvas (no gesture, or touch owns it). */
export function gestureChange(
  state: SafariGestureState,
  scale: number,
  touchActive: boolean,
  env: GestureEnv,
): ZoomTransform | null {
  if (!state.active || !state.baseline || touchActive) return null;
  const { transform, anchor } = state.baseline;
  return zoomToAt(transform, transform.scale * scale, anchor, env.content, env.viewport);
}

export function gestureEnd(state: SafariGestureState, now: number): SafariGestureState {
  return { ...state, active: false, endedAt: now, baseline: null };
}

// Safari fires a stray ctrlKey wheel just after `gestureend`.
const GESTURE_ECHO_WINDOW_MS = 100;

/**
 * WebKit mode only: whether a `wheel` is the echo of a trackpad pinch that the
 * `gesture*` events already handle, so it must not zoom a second time.
 */
export function shouldIgnoreWebkitWheel(
  state: SafariGestureState,
  event: Pick<WheelLike, "deltaY" | "ctrlKey" | "metaKey">,
  now: number,
): boolean {
  if (event.metaKey || !event.ctrlKey) return false;
  if (state.active) return true;
  if (state.endedAt !== null && now - state.endedAt <= GESTURE_ECHO_WINDOW_MS) return true;
  // A mouse wheel reports whole pixels; fractional deltas mean a trackpad.
  return !Number.isInteger(event.deltaY);
}
